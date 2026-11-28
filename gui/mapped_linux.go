package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// mappedKB is the resident size of the memory mapping that holds addr, from
// /proc/self/smaps; -1 if none does. Only e2e and tests read it.
func mappedKB(addr uintptr) int {
	f, err := os.Open("/proc/self/smaps")
	if err != nil {
		return -1
	}
	defer f.Close()
	in := false
	for s := bufio.NewScanner(f); s.Scan(); {
		line := s.Text()
		var lo, hi uintptr
		if n, _ := fmt.Sscanf(line, "%x-%x", &lo, &hi); n == 2 {
			in = lo <= addr && addr < hi
			continue
		}
		if in && strings.HasPrefix(line, "Rss:") {
			kb, _ := strconv.Atoi(strings.Fields(line)[1])
			return kb
		}
	}
	return -1
}
