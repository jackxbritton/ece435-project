package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

type ID int

type Server struct {
	mutex          sync.Mutex
	players        []Player
	count          int
	submissionChan chan submission
}

type Player struct {
	id          ID
	active      bool
	messageChan chan string
}

type submission struct {
	id ID
}

func newServer(cap int) *Server {
	s := &Server{}
	s.players = make([]Player, cap)
	s.submissionChan = make(chan submission)
	return s
}

func (s *Server) Start() error {

	go func() {

		turn := 0

		for {

			s.mutex.Lock()

			if s.count == 0 {
				time.Sleep(time.Second)
				s.mutex.Unlock()
				continue
			}

			if !s.players[turn].active {
				s.mutex.Unlock()
				turn = (turn + 1) % len(s.players)
				continue
			}

			// TODO How to do a timeout on a write?
			// What if a malicious user hasn't opened
			// the status endpoint?
			s.players[turn].messageChan <- "your turn"

			fmt.Println(turn)

			// TODO Wait for the response.
			time.Sleep(1 * time.Second)

			// Go to the next player.
			turn = (turn + 1) % len(s.players)

			s.mutex.Unlock()

		}

	}()

	http.HandleFunc("/", s.homeHandler)
	http.HandleFunc("/status", s.statusHandler)
	http.HandleFunc("/submit", s.submitHandler)
	return http.ListenAndServe(":8080", nil)

}

func (s *Server) newPlayer() *Player {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	for id := range s.players {
		if !s.players[id].active {
			s.players[id].active = true
			s.players[id].id = ID(id)
			s.players[id].messageChan = make(chan string)
			s.count++
			return &s.players[id]
		}
	}
	return nil
}

func (s *Server) deletePlayer(p *Player) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	p.active = false
	p.messageChan = nil
}

func (s *Server) homeHandler(w http.ResponseWriter, r *http.Request) {

	// Serve index.html.
	file, err := os.Open("index.html")
	if err != nil {
		log.Println(err)
		return
	}
	defer file.Close()

	if _, err := io.Copy(w, file); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println(err)
		return
	}

}

func (s *Server) statusHandler(w http.ResponseWriter, r *http.Request) {

	// Create a new Player.
	p := s.newPlayer()
	if p == nil {
		fmt.Fprintf(w, "Sorry we're full.") // TODO Better error.
		return
	}
	defer s.deletePlayer(p)

	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println("flushing not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Send the user their id.
	// TODO Use templated HTML instead.
	fmt.Fprintf(w, "event: id\r\ndata: %d\r\n\r\n", p.id)
	flusher.Flush()

	stop := w.(http.CloseNotifier).CloseNotify()

	for {
		select {
		case <-stop:
			return
		case x := <-p.messageChan:
			fmt.Fprintf(w, "data: %s\r\n\r\n", x)
			flusher.Flush()
		}
	}

}

func (s *Server) submitHandler(w http.ResponseWriter, r *http.Request) {

}
