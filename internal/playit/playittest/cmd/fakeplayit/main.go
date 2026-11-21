// fakeplayit runs the stand-in playit.gg API on a local port, so ZeroOS can
// be tried end to end without a playit account:
//
//	go run ./internal/playit/playittest/cmd/fakeplayit 127.0.0.1:9911
//	ZEROOS_PLAYIT_API=http://127.0.0.1:9911 zeroos serve
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/MightyX3N/zeroos/internal/playit/playittest"
)

func main() {
	addr := "127.0.0.1:9911"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	log.Printf("fake playit API on http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, playittest.New()))
}
