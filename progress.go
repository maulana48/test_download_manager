package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

type progressBar struct {
	p map[int64]*progress
	*sync.RWMutex
}

type progress struct {
	curr  int64 // curr is the current read till now
	total int64 // total bytes which we are supposed to read
}

var progressSize int

func (sum *summon) startProgressBar(wg *sync.WaitGroup, stop chan struct{}) {

	defer wg.Done()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			for i := int64(0); i < int64(len(sum.progressBar.p)); i++ {

				sum.progressBar.RLock()
				p := *sum.progressBar.p[i]
				sum.progressBar.RUnlock()

				printProgress(i, p)
			}

			// Move cursor back
			for i := 0; i < len(sum.progressBar.p); i++ {
				fmt.Print("\033[F")
			}

		case <-stop:
			for i := int64(0); i < int64(len(sum.progressBar.p)); i++ {
				sum.progressBar.RLock()
				p := *sum.progressBar.p[i]
				sum.progressBar.RUnlock()
				printProgress(i, p)
			}
			return
		}
	}

}

func printProgress(index int64, p progress) {

	s := strings.Builder{}

	percent := math.Round((float64(p.curr) / float64(p.total)) * 100)

	n := int((percent / 100) * float64(progressSize))

	s.WriteString("[")
	for i := 0; i < progressSize; i++ {
		if i <= n {
			s.WriteString(">")
		} else {
			s.WriteString(" ")
		}
	}
	s.WriteString("]")
	s.WriteString(fmt.Sprintf(" %v%%", percent))

	fmt.Printf("Connection %d  - %s\n", index+1, s.String())
}
