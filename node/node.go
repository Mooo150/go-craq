// node package corresponds to what the CRAQ white paper refers to as a node.

package node

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/rpc"
	"sync"

	"github.com/despreston/go-craq/craqrpc"
	"github.com/despreston/go-craq/transport"
	"golang.org/x/sync/errgroup"
)

var (
	// ErrNotFound should be returned by storage during a read operation if no
	// item exists for the given key.
	ErrNotFound = errors.New("that key does not exist")

	// ErrDirtyItem should be returned by storage if the latest version for the
	// key has not been committed yet.
	ErrDirtyItem = errors.New("key has an uncommitted version")
)

// neighbor is another node in the chain
type neighbor struct {
	client transport.Client
	path   string
}

// Item is an object in the Store. A key inside the store might have multiple
// versions.
type Item struct {
	Version   uint64
	Committed bool
	Value     []byte
	Key       string
}

type storer interface {
	Read(string) (*Item, error)
	Write(string, []byte, uint64) error
	Commit(string, uint64) error
	ReadVersion(string, uint64) (*Item, error)
	AllNewerCommitted(map[string][]uint64) ([]*Item, error)
	AllNewerDirty(map[string][]uint64) ([]*Item, error)
	AllDirty() ([]*Item, error)
	AllCommitted() ([]*Item, error)
}

// Opts is for passing options to the Node constructor.
type Opts struct {
	Store     storer
	Path      string
	CdrPath   string
	Transport transport.Transporter
}

// Node is what the white paper refers to as a node. This is the client that is
// responsible for storing data and handling reads/writes.
type Node struct {
	neighbors      map[craqrpc.NeighborPos]neighbor // other nodes in the chain
	store          storer                           // storage layer
	latest         map[string]uint64                // latest version of a given key
	CdrPath        string                           // host + port to coordinator
	cdr            transport.Client
	Path           string // host + port for rpc communication
	isHead, isTail bool
	mu             sync.Mutex
	transport      transport.Transporter
}

// New creates a new Node.
func New(opts Opts) *Node {
	return &Node{
		latest:    make(map[string]uint64),
		neighbors: make(map[craqrpc.NeighborPos]neighbor, 3),
		CdrPath:   opts.CdrPath,
		Path:      opts.Path,
		store:     opts.Store,
		transport: opts.Transport,
	}
}

// ListenAndServe starts listening for messages and connects to the coordinator.
func (n *Node) ListenAndServe() error {
	nRPC := &RPC{n}
	rpc.Register(nRPC)
	rpc.HandleHTTP()

	errg := errgroup.Group{}
	server := &http.Server{Addr: n.Path}

	errg.Go(server.ListenAndServe)

	errg.Go(func() error {
		err := n.ConnectToCoordinator()
		if err != nil {
			log.Println(err.Error())
			server.Shutdown(context.Background())
		}
		return err
	})

	return errg.Wait()
}

// ConnectToCoordinator let's the Node announce itself to the chain coordinator
// to be added to the chain. The coordinator responds with a message to tell the
// Node if it's the head or tail, and with the path of the previous node in the
// chain and the path to the tail node. The Node announces itself to the
// neighbor using the path given by the coordinator.
func (n *Node) ConnectToCoordinator() error {
	cdrClient, err := n.transport.Connect(n.CdrPath)
	if err != nil {
		log.Println("Error connecting to the coordinator")
		return err
	}

	log.Printf("Connected to coordinator at %s\n", n.CdrPath)
	n.cdr = cdrClient

	// Announce self to the Coordinatorr
	reply := craqrpc.NodeMeta{}
	if err := cdrClient.Call("RPC.AddNode", n.Path, &reply); err != nil {
		return err
	}

	n.isHead = reply.IsHead
	n.isTail = reply.IsTail
	n.neighbors[craqrpc.NeighborPosTail] = neighbor{path: reply.Tail}

	// Connect to tail
	if !reply.IsTail {
		if err := n.connectToNode(reply.Tail, craqrpc.NeighborPosTail); err != nil {
			log.Println("Error connecting to the tail during ConnectToCoordinator")
			return err
		}
	}

	// Connect to predecessor
	if reply.Prev != "" {
		if err := n.connectToNode(reply.Prev, craqrpc.NeighborPosPrev); err != nil {
			log.Printf("Failed to connect to node in ConnectToCoordinator. %v\n", err)
			return err
		}
		if err := n.fullPropagate(); err != nil {
			return err
		}
	} else if n.neighbors[craqrpc.NeighborPosPrev].path != "" {
		// Close the connection to the previous predecessor.
		n.neighbors[craqrpc.NeighborPosPrev].client.Close()
	}

	return nil
}

// send FwdPropagate and BackPropagate requests to new predecessor to get fully
// caught up. Forward propagation should go first so that it has all the dirty
// items needed before receiving backwards propagation response.
func (n *Node) fullPropagate() error {
	prevNeighbor := n.neighbors[craqrpc.NeighborPosPrev].client
	if err := n.requestFwdPropagation(&prevNeighbor); err != nil {
		return err
	}
	return n.requestBackPropagation(&prevNeighbor)
}

func (n *Node) connectToNode(path string, pos craqrpc.NeighborPos) error {
	client, err := n.transport.Connect(path)
	if err != nil {
		return err
	}

	log.Printf("connected to %s\n", path)

	// Disconnect from current neighbor if there's one connected.
	nbr := n.neighbors[pos]
	if nbr.client != nil {
		nbr.client.Close()
	}

	n.neighbors[pos] = neighbor{
		client: client,
		path:   path,
	}

	return nil
}

func (n *Node) writePropagated(reply *craqrpc.PropagateResponse) error {
	// Save items from reply to store.
	for key, forKey := range *reply {
		for _, item := range forKey {
			if err := n.store.Write(key, item.Value, item.Version); err != nil {
				log.Printf("Failed to write item %+v to store: %#v\n", item, err)
				return err
			}
		}
	}
	return nil
}

func (n *Node) commitPropagated(reply *craqrpc.PropagateResponse) error {
	// Commit items from reply to store.
	for key, forKey := range *reply {
		for _, item := range forKey {
			if err := n.store.Commit(key, item.Version); err != nil {
				log.Printf("Failed to commit item %+v to store: %#v\n", item, err)
				return err
			}
		}
	}
	return nil
}

func propagateRequestFromItems(items []*Item) craqrpc.PropagateRequest {
	req := craqrpc.PropagateRequest{}
	for _, item := range items {
		req[item.Key] = append(req[item.Key], item.Version)
	}
	return req
}

// requestFwdPropagation asks client to respond with all uncommitted (dirty)
// items that this node either does not have or are newer than what this node
// has.
func (n *Node) requestFwdPropagation(client *transport.Client) error {
	dirty, err := n.store.AllDirty()
	if err != nil {
		log.Printf("Failed to get all dirty items: %#v\n", err)
		return err
	}

	reply := craqrpc.PropagateResponse{}
	args := propagateRequestFromItems(dirty)
	if err := (*client).Call("RPC.FwdPropagate", &args, &reply); err != nil {
		log.Printf("Failed during forward propagation: %#v\n", err)
		return err
	}

	return n.writePropagated(&reply)
}

// requestBackPropagation asks client to respond with all committed items that
// this node either does not have or are newer than what this node has.
func (n *Node) requestBackPropagation(client *transport.Client) error {
	committed, err := n.store.AllCommitted()
	if err != nil {
		log.Printf("Failed to get all committed items: %#v\n", err)
		return err
	}

	args := propagateRequestFromItems(committed)
	reply := craqrpc.PropagateResponse{}
	if err := (*client).Call("RPC.BackPropagate", &args, &reply); err != nil {
		log.Printf("Failed during back propagation: %#v\n", err)
		return err
	}

	return n.commitPropagated(&reply)
}

// resetNeighbor closes any open connection and resets the object.
func resetNeighbor(n *neighbor) {
	n.client.Close()
	*n = neighbor{}
}
