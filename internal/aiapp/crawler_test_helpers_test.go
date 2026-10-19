package aiapp

import (
	"time"

	"go-lua-crawler/internal/acquisition"
)

// rowTestPage builds the inspection a fetch would produce, so crawler tests can
// drive the real reader over a page body without a network round trip.
func rowTestPage(body, url string) sourceInspection {
	return inspectResponse(
		acquisition.Response{
			Body:    body,
			Receipt: acquisition.Receipt{Method: "GET", URL: url, Status: 200, FetchedAt: time.Now()},
		},
		acquisition.Stats{Requests: 1, Bytes: int64(len(body))},
	)
}
