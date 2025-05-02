package main

import (
	"log"
	"os"
	"strconv"
	"time"
)

func main() {
	amount := os.Args[1]
	amountInt, err := strconv.Atoi(amount)
	if err != nil {
		log.Fatalf("Failed to convert amount to int: %v", err)
	}
	time.Sleep(time.Duration(amountInt) * time.Second)
}
