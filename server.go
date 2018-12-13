package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

type Server struct {
	router *mux.Router

	mutex   sync.Mutex
	players []Player
	count   int

	words     []string
	wordCount int
	story     string

	gameIsHappening bool

	responseChan      chan response
	playerJoiningChan chan int
	playerLeavingChan chan int
	resetChan         chan struct{}
}

type Player struct {
	ID          int `json:"id"`
	active      bool
	messageChan chan []byte
}

type response struct {
	playerID int
	WordID   int  `json:"id"`
	Reset    bool `json:"reset,omitempty"`
}

func NewServer(cap int) (*Server, error) {

	s := &Server{}
	s.players = make([]Player, cap)
	for i := range s.players {
		s.players[i].messageChan = make(chan []byte)
	}
	s.responseChan = make(chan response)
	s.playerJoiningChan = make(chan int)
	s.playerLeavingChan = make(chan int)
	s.resetChan = make(chan struct{})

	// Load words from their file.
	file, err := os.Open("words.txt")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		// First string is a number.
		strs := strings.Split(scanner.Text(), " ")
		count, err := strconv.Atoi(strs[0])
		if err != nil {
			log.Println(err)
			continue
		}
		if len(strs) < 2 {
			log.Println("bad line in words.txt!")
			continue
		}
		// Add <count> of the string to the words.
		word := strings.Join(strs[1:], " ")
		for i := 0; i < count; i++ {
			s.words = append(s.words, word)
		}
	}

	// Shuffle the words.
	rand.Shuffle(len(s.words), func(i, j int) {
		s.words[i], s.words[j] = s.words[j], s.words[i]
	})

	s.router = mux.NewRouter()
	s.router.HandleFunc("/", s.homeHandler)
	s.router.HandleFunc("/status", s.websocketHandler)
	s.router.HandleFunc("/audio/{filename}", func(w http.ResponseWriter, r *http.Request) {

		vars := mux.Vars(r)
		filename := vars["filename"]

		// Open the audio file.
		file, err := os.Open(filename)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			log.Println(err)
			return
		}
		defer file.Close()

		// Set header and copy.
		w.Header().Set("Content-Type", "audio/mpeg")
		if _, err = io.Copy(w, file); err != nil {
			log.Println(err)
			return
		}

	})

	return s, nil
}

func (s *Server) Start() error {
	http.Handle("/", s.router)
	return http.ListenAndServe(":80", nil)
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

// TODO Make not global.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

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
		conn.WriteMessage(websocket.TextMessage, []byte("Sorry we're full."))
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

	s.broadcastPlayers()

	// Listen for events.
	for {

		// Wait for a message.
		_, bytes, err := conn.ReadMessage()
		if err != nil {
			if _, ok := err.(*websocket.CloseError); !ok {
				log.Println(err)
			}
			return
		}

		// Serialize into a response and push to the channel.
		var res response
		if err := json.Unmarshal(bytes, &res); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			log.Println(err)
			return
		}
		res.playerID = p.ID
		s.responseChan <- res

	}

}

func (s *Server) broadcastPlayers() {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Assemble array of active players.
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

		// Select new words.
		type word struct {
			ID   int    `json:"id"`
			Word string `json:"word"`
		}
		var words []word
		wordsPerTurn := 10
		for i := 0; i < wordsPerTurn; i++ {
			words = append(words, word{
				ID:   i,
				Word: s.words[s.wordCount],
			})
			s.wordCount = (s.wordCount + 1) % len(s.words)
		}

		// Broadcast an update.
		update := struct {
			Words []word `json:"words,omitempty"`
			Turn  int    `json:"turn"`
			Story string `json:"story"`
		}{
			Words: words,
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

				// Remove the old player.
				s.mutex.Lock()
				s.players[id].active = false
				s.count--
				s.mutex.Unlock()

				// Broadcast current active players.
				s.broadcastPlayers()

				// If the current player just left, skip to the next turn.
				if turn == id {
					break loop
				}

			case res := <-s.responseChan:
				if res.Reset {
					s.story = ""
					turn = 0
					break loop
				}
				if res.playerID != turn {
					continue
				}
				if res.WordID < 0 || res.WordID >= wordsPerTurn {
					break loop
				}
				s.story = s.story + " " + words[res.WordID].Word
				break loop
			case <-ticker.C:
				break loop
			}
		}

		turn = (turn + 1) % len(s.players)
	}
}
