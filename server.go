package main

import (
	"bufio"
	"encoding/json"
	"fmt"
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
	submissionChan  chan submission
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

type Update struct {
	Players []Player `json:"players,omitempty"`
	Words   []word   `json:"words,omitempty"`
	Turn    int      `json:"turn"`
	Story   string   `json:"story"`
}

type word struct {
	ID   int    `json:"id"`
	Word string `json:"word"`
}

func NewServer(cap int) (*Server, error) {

	s := &Server{}
	s.players = make([]Player, cap)
	s.submissionChan = make(chan submission)

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
			s.players[id].messageChan = make(chan []byte)
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

	// Create a new Player.
	p := s.newPlayer()
	if p == nil {
		fmt.Fprintf(w, "Sorry we're full.") // TODO Better error.
		return
	}
	defer s.deletePlayer(p)

	// Listen for messages that the game thread want us to send.
	stop := make(chan struct{})
	defer func() {
		stop <- struct{}{}
	}()
	go func() {
		for {
			select {
			case <-stop:
				return
			case msg := <-p.messageChan:
				if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
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

	// Send the player their ID (and soon init data).
	idBytes, err := json.Marshal(struct {
		ID int `json:"id"`
	}{ID: p.ID})
	if err != nil {
		log.Println(err)
		return
	}
	p.messageChan <- idBytes

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

		s.mutex.Lock()

		// Kill the game if everyone left.
		if s.count == 0 {
			s.gameIsHappening = false
			s.mutex.Unlock()
			return
		}
		s.mutex.Unlock()

		// TODO Advance the turn so long as the current player is inactive.
		for {
			if s.players[turn].active {
				break
			}
			turn = (turn + 1) % len(s.players)
		}

		// Get an array of the active players.
		var activePlayers []Player
		for _, p := range s.players {
			if !p.active {
				continue
			}
			activePlayers = append(activePlayers, p)
		}

		// Broadcast an update.
		update := Update{
			Players: activePlayers,
			Words:   s.words,
			Turn:    turn,
			Story:   s.story,
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
		ticker := time.NewTicker(5 * time.Second)
	loop:
		for {
			select {
			case sub := <-s.submissionChan:
				if sub.playerID != turn {
					continue
				}
				if sub.WordID < 0 || sub.WordID >= len(s.words) {
					break loop
				}
				s.story = s.story + s.words[sub.WordID].Word
			case <-ticker.C:
				break loop
			}
		}

		turn = (turn + 1) % len(s.players)

	}

}
