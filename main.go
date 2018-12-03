package main

import (
	"log"
)

func main() {
	s, err := NewServer(4)
	if err != nil {
		log.Println(err)
		return
	}
	log.Println(s.Start())
}
