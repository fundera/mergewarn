package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"gopkg.in/fsnotify.v1"
	"gopkg.in/libgit2/git2go.v22"
	"gopkg.in/redis.v3"
)

// FileEdit convert maps from above into structs for encoding
type FileEdit struct {
	Filename    string `json:"filename"`
	LineNumbers []int  `json:"lineNumbers"`
}

// Configuration is the global configuration for mergewarn.
type Configuration struct {
	RedisURI      string `json:"RedisURI"`
	RepoDirectory string `json:"RepoDirectory"`
	CurrentUser   string `json:"CurrentUser"`
}

var config *Configuration

func initConfig() *Configuration {
	file, _ := os.Open("mergewarn.conf")
	decoder := json.NewDecoder(file)
	configuration := Configuration{}
	err := decoder.Decode(&configuration)
	if err != nil {
		fmt.Println("error:", err)
	}
	return &configuration
}

func initRedisClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     config.RedisURI,
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

func getLocalTree(repo *git.Repository) (*git.Tree, error) {
	rev, err := repo.RevparseSingle("origin/master^{tree}")
	if err != nil {
		log.Fatal(err)
	}

	tree, err := repo.LookupTree(rev.Id())
	if err != nil {
		log.Fatal(err)
	}

	return tree, err
}

func getLocalFileListing() []string {
	repo, err := git.OpenRepository(config.RepoDirectory)
	if err != nil {
		log.Fatal(err)
	}

	tree, err := getLocalTree(repo)
	var allFiles []string

	tree.Walk(func(str string, entry *git.TreeEntry) int {
		if entry.Type == git.ObjectBlob {
			fullPath := config.RepoDirectory + "/" + str + entry.Name
			allFiles = append(allFiles, fullPath)
		}

		return 0
	})

	return allFiles
}

func buildDiff() (*git.Diff, error) {
	repo, err := git.OpenRepository(config.RepoDirectory)
	if err != nil {
		log.Fatal(err)
	}

	tree, err := getLocalTree(repo)
	if err != nil {
		panic(err)
	}

	diff, err := repo.DiffTreeToWorkdir(tree, nil)
	if err != nil {
		log.Fatal(err)
	}

	return diff, err
}

func buildLocalFileEdits() []FileEdit {
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

	return sanitizedFileEdits
}

func notice(str string) {
	fmt.Println("*** " + str)
}

func sendAndNotifyChange(redisClient *redis.Client, jsonBody []byte) {
	redisClient.HSet("mergewarnDiffs", "fundera_"+config.CurrentUser, string(jsonBody))
	notice("Publishing changes..")
	redisClient.Publish("newChange", "1")
}

func processAllDiffs(redisClient *redis.Client) {
	allDiffs := redisClient.HGetAllMap("mergewarnDiffs")
	diffUserMap, err := allDiffs.Result()

	localFileEdits := buildLocalFileEdits()

	if err != nil {
		fmt.Println(err)
	}

	// {"filename":"frontend/stylesheets/bootstrap_application.css.sass","lineNumbers":[33]},{"filename":"package.json","lineNumbers":[1]}
	for user, diffSet := range diffUserMap {
		fileEdits := []FileEdit{}
		json.Unmarshal([]byte(diffSet), &fileEdits)

		// iterate through each file diff and create a notice if that user is editing that line. Oh no!

		for idx := range fileEdits {
			fileEdit := fileEdits[idx]

			for localIdx := range localFileEdits {
				localFileEdit := localFileEdits[localIdx]

				// also check for line number collision here
				if localFileEdit.Filename == fileEdit.Filename {

					for a := range localFileEdit.LineNumbers {
						for b := range fileEdit.LineNumbers {
							if localFileEdit.LineNumbers[a] == fileEdit.LineNumbers[b] {
								s := fmt.Sprintf("MERGE WARN!!! User: %s, Filename: %s, Line Number: %d", user, fileEdit.Filename, fileEdit.LineNumbers[b])
								notice(s)
							}
						}
					}

				}
			}
		}

		notice(user)
	}
}

func waitForServerChanges(redisClient *redis.Client) {
	// Process diff changes
	go func() {
		pubsub, err := redisClient.Subscribe("newChange")
		if err != nil {
			panic("ERROR: Cannot connect to redis server. Make sure it is running at " + config.RedisURI)
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
					fileEdits := buildLocalFileEdits()
					jsonBody, err := json.Marshal(fileEdits)

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
	allFiles := getLocalFileListing()

	for i := range allFiles {
		watchErr := watcher.Add(allFiles[i])
		if watchErr != nil {
			log.Fatal(watchErr)
		}
		dirsAdded = dirsAdded + 1
	}

	str := fmt.Sprintf("Watching %d directories.", dirsAdded)
	notice(str)

	<-done

	watcher.Close()
}

// StartClient starts the mergewarn client and sends data to the server periodically.
func main() {
	config = initConfig()
	redisClient := initRedisClient()

	waitForServerChanges(redisClient)
	waitForLocalChanges(redisClient)
}
