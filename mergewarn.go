package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"reflect"
	"time"

	"gopkg.in/libgit2/git2go.v22"
	"gopkg.in/redis.v3"
)

// FileEdit convert maps from above into structs for encoding
type FileEdit struct {
	Filename    string `json:"filename"`
	LineNumbers []int  `json:"lineNumbers"`
	User        string `json:"user"`
}

// Configuration is the global configuration for mergewarn.
type Configuration struct {
	RedisURI      string `json:"RedisURI"`
	RepoDirectory string `json:"RepoDirectory"`
	CurrentUser   string `json:"CurrentUser"`
	RedisPassword string `json:"RedisPassword"`
	TestMode      bool   `json:"TestMode"`
}

var config *Configuration

func initConfig() *Configuration {
	file, _ := os.Open("mergewarn.conf.js")
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
		Password: config.RedisPassword,
		DB:       0, // use default DB
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
	repo, err := git.OpenRepository(config.RepoDirectory)
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
	fmt.Print(time.Now())
	fmt.Print("|")
	fmt.Print(str)
	fmt.Print("\n")
}

func sendAndNotifyChange(redisClient *redis.Client, jsonBody []byte) {
	redisClient.HSet("mergewarnDiffs", config.CurrentUser, string(jsonBody))
	redisClient.Publish("newChange", "1")
}

func calculateConflicts(redisClient *redis.Client) (conflictFileEdits []FileEdit) {
	allDiffs := redisClient.HGetAllMap("mergewarnDiffs")
	diffUserMap, err := allDiffs.Result()

	localFileEdits := buildLocalFileEdits()

	if err != nil {
		fmt.Println(err)
	}

	// {"filename":"frontend/stylesheets/bootstrap_application.css.sass","lineNumbers":[33]},{"filename":"package.json","lineNumbers":[1]}
	for user, diffSet := range diffUserMap {

		// Only process diffs if it isn't the current user or we aren't in test mode.
		// Test Mode would allow a single user to test the app.
		//
		if user != config.CurrentUser || config.TestMode {
			incomingFileEdits := []FileEdit{}
			json.Unmarshal([]byte(diffSet), &incomingFileEdits)

			// iterate through each file diff and create a notice if that user is editing that line. Oh no!

			for idx := range incomingFileEdits {
				fileEdit := incomingFileEdits[idx]

				for localIdx := range localFileEdits {
					localFileEdit := localFileEdits[localIdx]

					// also check for line number collision here
					if localFileEdit.Filename == fileEdit.Filename {

						for a := range localFileEdit.LineNumbers {
							for b := range fileEdit.LineNumbers {
								if localFileEdit.LineNumbers[a] == fileEdit.LineNumbers[b] {
									localFileEdit.User = user
									conflictFileEdits = append(conflictFileEdits, localFileEdit)
								}
							}
						}
					}
				}
			}
		}
	}

	return conflictFileEdits
}

func outputConflicts(conflicts []FileEdit) {
	jsonBody, err := json.Marshal(conflicts)

	if err != nil {
		log.Fatal(err)
	}

	notice(string(jsonBody))
}

func waitForServerChanges(redisClient *redis.Client) {
	pubsub, err := redisClient.Subscribe("newChange")
	if err != nil {
		panic("ERROR: Cannot connect to redis server. Make sure it is running at " + config.RedisURI)
	}
	defer pubsub.Close()

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
			fetchedConflicts := calculateConflicts(redisClient)
			outputConflicts(fetchedConflicts)
		case *redis.Pong:
			fmt.Println(msg)
		default:
			panic(fmt.Sprintf("unknown message: %#v", msgi))
		}
	}
}

func waitForLocalChanges(redisClient *redis.Client) {
	lastFileEdits := []FileEdit{}

	for {
		fileEdits := buildLocalFileEdits()

		if !reflect.DeepEqual(lastFileEdits, fileEdits) {
			jsonBody, err := json.Marshal(fileEdits)

			if err != nil {
				log.Fatal(err)
			}
			sendAndNotifyChange(redisClient, jsonBody)
			lastFileEdits = fileEdits
		}
		time.Sleep(5 * time.Second)
	}
}

func main() {
	fmt.Println("------------------------------")
	fmt.Println("MergeWarn listener starting...")
	fmt.Println("------------------------------")
	config = initConfig()
	redisClient := initRedisClient()

	go waitForServerChanges(redisClient)
	waitForLocalChanges(redisClient)
}
