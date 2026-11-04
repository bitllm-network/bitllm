package main

import (
	"fmt"
	"os"

	"github.com/bitllm-network/bitllm/internal/models"
)

// hashModelCmd is `bitllm hash-model DIR`.
func hashModelCmd(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: bitllm hash-model DIR\n\nPrints the hash of the weight files under DIR, for --weights NAME=HASH.")
		os.Exit(2)
	}
	sum, n, err := models.HashModel(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s  (%d files)\n", sum, n)
	for name, mi := range models.Manifest {
		if mi.Weights == sum {
			fmt.Printf("matches %s in models.json\n", name)
		}
	}
}
