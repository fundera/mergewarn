package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"gopkg.in/fsnotify.v1"
	"gopkg.in/libgit2/git2go.v22"
	"gopkg.in/redis.v3"
)

const repoDirectory = "/Users/rdeshpande/dev/fundera"
const redisURI = "localhost:6379"
const currentUser = "rohan"

// FileEdit convert maps from above into structs for encoding
type FileEdit struct {
	Filename    string `json:"filename"`
	LineNumbers []int  `json:"lineNumbers"`
}

func initRedisClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     redisURI,
		Password: "", // no password set
		DB:       0,  // use default DB
	})
}

func parseDiff(diff *git.Diff) map[string]map[int]bool {
	fileEdits := make(map[string]map[int]bool)

	_ = diff.ForEach(func(file git.DiffDelta, progress float64) (git.DiffForEachHunkCallback, error) {
		return func(hunk git.DiffHunk) (git.DiffForEachLineCallback, error) {
			return func(line git.DiffLine) error {
				if line.Origin == git.DiffLineAddition || line.Origin == git.DiffLineDeletion {
					var lineNumber int
					if line.NewLineno > 0 {
						lineNumber = line.NewLineno
					} else {
						lineNumber = line.OldLineno
					}

					path := file.OldFile.Path

					if fileEdits[path] == nil {
						fileEdits[path] = make(map[int]bool)
					}

					fileEdits[path][lineNumber] = true
				}
				return nil
			}, nil

		}, nil
	}, git.DiffDetailLines)

	return fileEdits
}

func buildDiff() (*git.Diff, error) {
	repo, err := git.OpenRepository(repoDirectory)
	if err != nil {
		log.Fatal(err)
	}

	rev, err := repo.RevparseSingle("origin/master^{tree}")
	if err != nil {
		log.Fatal(err)
	}

	tree, err := repo.LookupTree(rev.Id())
	if err != nil {
		log.Fatal(err)
	}

	diff, err := repo.DiffTreeToWorkdir(tree, nil)
	if err != nil {
		log.Fatal(err)
	}

	return diff, err
}

func buildAndParseDiff() ([]byte, error) {
	diff, err := buildDiff()
	if err != nil {
		log.Fatal(err)
	}
	fileEdits := parseDiff(diff)

	sanitizedFileEdits := []FileEdit{}

	for tempFilename, lineNumberMap := range fileEdits {
		f := FileEdit{}
		f.Filename = tempFilename

		for l := range lineNumberMap {
			f.LineNumbers = append(f.LineNumbers, l)
		}

		sanitizedFileEdits = append(sanitizedFileEdits, f)
	}

	jsonBody, err := json.Marshal(sanitizedFileEdits)
	return jsonBody, err
}

func notice(str string) {
	fmt.Println("*** " + str)
}

func sendAndNotifyChange(redisClient *redis.Client, jsonBody []byte) {
	redisClient.HSet("mergewarnDiffs", "fundera_"+currentUser, string(jsonBody))
	notice("Publishing changes..")
	redisClient.Publish("newChange", "1")
}

func processAllDiffs(redisClient *redis.Client) {
	allDiffs := redisClient.HGetAllMap("mergewarnDiffs")

	diffMap, err := allDiffs.Result()
	if err != nil {
		fmt.Println(err)
	}

	usersInMap := ""
	masterDiff := make(map[string]map[int]bool)

	// MERGE DIFFS

	// {"filename":"frontend/stylesheets/bootstrap_application.css.sass","lineNumbers":[33]},{"filename":"package.json","lineNumbers":[1]}
	for user, diff := range diffMap {
		fmt.Println(diff)
		usersInMap += user
		usersInMap += ","
	}

	notice("Message Received: " + usersInMap)
}

func waitForServerChanges(redisClient *redis.Client) {
	// Process diff changes
	go func() {
		pubsub, err := redisClient.Subscribe("newChange")
		if err != nil {
			panic("ERROR: Cannot connect to redis server. Make sure it is running at " + redisURI)
		}
		defer pubsub.Close()
		notice("Waiting for changes..")

		for {
			msgi, err := pubsub.Receive()

			if err != nil {
				err := pubsub.Ping("")
				if err != nil {
					panic(err)
				}
			}

			switch msg := msgi.(type) {
			case *redis.Subscription:
			case *redis.Message:
				processAllDiffs(redisClient)
			case *redis.Pong:
				fmt.Println(msg)
			default:
				panic(fmt.Sprintf("unknown message: %#v", msgi))
			}
		}
	}()
}

func waitForLocalChanges(redisClient *redis.Client) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}

	done := make(chan bool)

	// Process watch events
	go func() {
		for {
			select {
			case ev := <-watcher.Events:
				if ev.Op != fsnotify.Chmod {
					jsonBody, err := buildAndParseDiff()
					if err != nil {
						log.Fatal(err)
					}
					sendAndNotifyChange(redisClient, jsonBody)
				}

			case err := <-watcher.Errors:
				log.Println("error:", err)
			}
		}
	}()

	dirsAdded := 0

	// Gather ALL DIRS RECURSIVELY using filepath.Walk and the FileInfo isDir
	filepath.Walk(repoDirectory, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			watchErr := watcher.Add(repoDirectory)
			if watchErr != nil {
				log.Fatal(watchErr)
			}
			dirsAdded = dirsAdded + 1
		}
		return err
	})

	str := fmt.Sprintf("Added %d directories!", dirsAdded)
	notice(str)

	<-done

	watcher.Close()
}

// StartClient starts the mergewarn client and sends data to the server periodically.
func main() {
	redisClient := initRedisClient()

	waitForServerChanges(redisClient)
	waitForLocalChanges(redisClient)
}
