package main

import (
	"log"
)

func main() {
	s := newServer(4)
	log.Fatal(s.Start())
}
