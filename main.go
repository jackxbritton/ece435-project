package main

import (
	"log"
)

func main() {
	s, err := NewServer(32)
	if err != nil {
		log.Println(err)
		return
	}
	log.Println(s.Start())
}
