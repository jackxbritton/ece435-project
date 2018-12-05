package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Server struct {
	mutex   sync.Mutex
	players []Player
	count   int

	words []word
	story string

	gameIsHappening bool

	submissionChan    chan submission
	playerJoiningChan chan int
	playerLeavingChan chan int
}

type Player struct {
	ID          int `json:"id"`
	active      bool
	messageChan chan []byte
}

type submission struct {
	playerID int
	WordID   int `json:"id"`
}

type word struct {
	ID   int    `json:"id"`
	Word string `json:"word"`
}

func NewServer(cap int) (*Server, error) {

	s := &Server{}
	s.players = make([]Player, cap)
	for i := range s.players {
		s.players[i].messageChan = make(chan []byte)
	}
	s.submissionChan = make(chan submission)
	s.playerJoiningChan = make(chan int)
	s.playerLeavingChan = make(chan int)

	// Load words from their file.
	file, err := os.Open("words.txt")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	id := 0
	for scanner.Scan() {
		w := word{
			ID:   id,
			Word: string(scanner.Bytes()),
		}
		s.words = append(s.words, w)
		id++
	}

	return s, nil
}

func (s *Server) Start() error {
	http.HandleFunc("/", s.homeHandler)
	http.HandleFunc("/status", s.websocketHandler)
	return http.ListenAndServe(":8080", nil)
}

func (s *Server) newPlayer() *Player {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	for id := range s.players {
		if !s.players[id].active {
			s.players[id].active = true
			s.players[id].ID = id
			s.count++
			return &s.players[id]
		}
	}
	return nil
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

var upgrader = websocket.Upgrader{} // TODO Make not global.

func (s *Server) websocketHandler(w http.ResponseWriter, r *http.Request) {

	// Upgrade to a websocket connection.
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Println(err)
		return
	}
	defer conn.Close()

	// Create a new player.
	p := s.newPlayer()
	if p == nil {
		conn.WriteMessage(websocket.TextMessage, []byte("Sorry we're full.")) // TODO Better error.
		return
	}
	defer func() {
		s.playerLeavingChan <- p.ID
	}()

	// Launch thread to write messages.
	stop := make(chan struct{})
	defer func() {
		stop <- struct{}{}
	}()
	go func() {
		for {
			select {
			case <-stop:
				return
			case bytes := <-p.messageChan:
				err := conn.WriteMessage(websocket.TextMessage, bytes)
				if err != nil {
					log.Println(err)
					return
				}
			}
		}
	}()

	// If a game isn't happening, start one.
	s.mutex.Lock()
	if !s.gameIsHappening {
		s.gameIsHappening = true
		go s.startGame()
	}
	s.mutex.Unlock()

	// Notify game thread that player joined
	// (this must happen in this order).
	s.playerJoiningChan <- p.ID

	// Send the player their ID.
	idBytes, err := json.Marshal(struct {
		ID int `json:"id"`
	}{ID: p.ID})
	if err != nil {
		log.Println(err)
		return
	}
	p.messageChan <- idBytes

	// Broadcast the new player.
	var activePlayers []Player
	for _, player := range s.players {
		if !player.active {
			continue
		}
		activePlayers = append(activePlayers, player)
	}
	playerBytes, err := json.Marshal(struct {
		Players []Player `json:"players"`
	}{Players: activePlayers})
	if err != nil {
		log.Println(err)
		return
	}
	for _, player := range activePlayers {
		player.messageChan <- playerBytes
	}

	// Listen for events.
	for {

		// Wait for a message.
		_, bytes, err := conn.ReadMessage()
		if err != nil {
			log.Println(err) // TODO Handle disconnect error separately.
			return
		}

		// Serialize into a submission and push to the channel.
		var sub submission
		if err := json.Unmarshal(bytes, &sub); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			log.Println(err)
			return
		}
		sub.playerID = p.ID
		s.submissionChan <- sub

	}

}

func (s *Server) startGame() {

	turn := 0

	for {

		// Kill the game if everyone left.
		s.mutex.Lock()
		if s.count == 0 {
			s.gameIsHappening = false
			s.mutex.Unlock()
			return
		}
		s.mutex.Unlock()

		// Advance the turn until it falls on a player who's active.
		for {
			if s.players[turn].active {
				break
			}
			turn = (turn + 1) % len(s.players)
		}

		// Broadcast an update.
		update := struct {
			Words []word `json:"words,omitempty"`
			Turn  int    `json:"turn"`
			Story string `json:"story"`
		}{
			Words: s.words,
			Turn:  turn,
			Story: s.story,
		}
		updateBytes, err := json.Marshal(update)
		if err != nil {
			log.Println(err)
			return
		}
		for _, p := range s.players {
			if !p.active {
				continue
			}
			p.messageChan <- updateBytes
		}

		// Expect a response from the current player.
		// TODO Also handle new players here.
		ticker := time.NewTicker(10 * time.Second)
	loop:
		for {
			select {
			case id := <-s.playerJoiningChan:
				if id < 0 || id >= len(s.players) {
					continue
				}
				s.players[id].messageChan <- updateBytes
			case id := <-s.playerLeavingChan:
				if id < 0 || id >= len(s.players) {
					continue
				}
				s.players[id].active = false
				s.count--
				if turn == id {
					break loop
				}
			case sub := <-s.submissionChan:
				if sub.playerID != turn {
					continue
				}
				if sub.WordID < 0 || sub.WordID >= len(s.words) {
					break loop
				}
				s.story = s.story + " " + s.words[sub.WordID].Word
				break loop
			case <-ticker.C:
				break loop
			}
		}

		turn = (turn + 1) % len(s.players)

	}

}
