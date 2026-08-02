package main

// Runtime reverse-solver probe: replicates the platform bucket formula at BOTH
// the combo-layer level and the holdout-opt-experiment level, using the node ids
// read back from the live API as effective salts (combo nodes carry empty salt →
// bucket/strategy.go:122 falls back to the node ID).
import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/cespare/xxhash/v2"
)

func bucketOf(uid, salt string, total int64) int64 {
	if total <= 0 {
		return -1
	}
	return int64(xxhash.Sum64String(uid+"-"+salt) % uint64(total))
}

type rng struct{ Lo, Hi int64 }

func in(b int64, r rng) bool { return b >= r.Lo && b <= r.Hi }

func main() {
	comboLayer := os.Args[1] // effective salt of combo layer (id)
	hoExp := os.Args[2]      // effective salt of holdout-opt experiment (id)
	hoLo, hoHi := int64(0), int64(999)
	groups := map[string]rng{
		"h1": {0, 44999},
		"o1": {45000, 89999},
	}
	want := os.Args[3] // "h1" | "o1" | "simple"
	n := 0
	out := []string{}
	for i := 0; n < 3 && i < 400000; i++ {
		uid := fmt.Sprintf("st9-probe-%d", i)
		lb := bucketOf(uid, comboLayer, 10000)
		hoHit := in(lb, rng{hoLo, hoHi})
		if want == "simple" {
			if !hoHit {
				out = append(out, uid)
				n++
			}
			continue
		}
		if !hoHit {
			continue
		}
		eb := bucketOf(uid, hoExp, 90000)
		if in(eb, groups[want]) {
			out = append(out, uid)
			n++
		}
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
}
