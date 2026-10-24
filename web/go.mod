// This go.mod holds no Go code. It only marks web/ as outside the
// controller's module, so `go build ./...` and `go vet ./...` never walk
// into node_modules (some npm packages ship .go files).
module github.com/sharddevs/waystone_panel/web

go 1.27
