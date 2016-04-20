package main

import (
	"flag"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/tthanh/dkvs/raft"
)

var consumer chan raft.RPC

func main() {
	var new bool
	var addr string
	var join string

	flag.BoolVar(&new, "n", false, "new server")
	flag.StringVar(&addr, "a", "localhost:8080", "server address")
	flag.StringVar(&join, "j", "", "peers")

	flag.Parse()

	var server *raft.Server
	var r = mux.NewRouter()

	if new {
		consumer = make(chan raft.RPC)
		config := raft.DefaultConfig()
		transport := NewHTTPTransport(addr, consumer)
		ls := raft.NewInmemLogStore()
		sm := NewStateMachine()
		server = raft.NewServer(config, transport, ls, sm)
		if len(join) > 0 {
			peers := strings.Split(join, ",")
			for _, peer := range peers {
				server.AddPeer(peer)
			}
		}
		server.Start()
		defer server.Stop()

		r.HandleFunc("/request_vote", transport.requestVoteHandle(consumer)).Methods("POST")
		r.HandleFunc("/store/{key}", transport.getHandle(server)).Methods("GET")
		r.HandleFunc("/store/{key}", transport.setHandle(server)).Methods("POST")
		http.ListenAndServe(addr, r)
	}
}
