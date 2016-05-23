package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"reflect"
	"sort"
	"time"

	"gopkg.in/libgit2/git2go.v22"
	"gopkg.in/redis.v3"
)

// FileEdit convert maps from above into structs for encoding
type FileEdit struct {
	Filename    string `json:"filename"`
	LineNumbers []int  `json:"lineNumbers"`
	User        string `json:"user"`
	Branch      string `json:"branch"`
}

var redisURI = flag.String("uri", "localhost:6379", "Specify the Redis URI")
var repoDirectory = flag.String("dir", ".", "Directory of the repository to track")
var currentUser = flag.String("user", "", "Git user to trace back to")
var redisPassword = flag.String("redispw", "", "Redis Password")

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

func getTreeRev(repo *git.Repository, branchName string) *git.Tree {
	rev, err := repo.RevparseSingle(branchName + "^{tree}")
	if err != nil {
		log.Fatal(err)
	}

	tree, err := repo.LookupTree(rev.Id())
	if err != nil {
		log.Fatal(err)
	}

	return tree
}

func buildDiff(repo *git.Repository, branchName string) (*git.Diff, error) {
	masterTree := getTreeRev(repo, "master")

	if branchName == "master" {
		// If we are working on master, then just diff against
		// the current tree.
		// TODO: is WithIndex the right thing here?
		diff, err := repo.DiffTreeToWorkdirWithIndex(masterTree, nil)
		if err != nil {
			log.Fatal(err)
		}
		return diff, err
	}

	otherTree := getTreeRev(repo, branchName)
	opts, err := git.DefaultDiffOptions()
	diff, err := repo.DiffTreeToTree(masterTree, otherTree, &opts)
	if err != nil {
		log.Fatal(err)
	}
	return diff, err
}

func buildLocalFileEdits() []FileEdit {
	repo, err := git.OpenRepository(*repoDirectory)
	if err != nil {
		log.Fatal(err)
	}
	head, _ := repo.Head()
	branchName, _ := head.Branch().Name()

	diff, _ := buildDiff(repo, branchName)
	fileEdits := parseDiff(diff)

	sanitizedFileEdits := []FileEdit{}

	for tempFilename, lineNumberMap := range fileEdits {
		f := FileEdit{}
		f.Filename = tempFilename
		f.User = *currentUser
		f.Branch = branchName

		for l := range lineNumberMap {
			f.LineNumbers = append(f.LineNumbers, l)
		}

		sort.Ints(f.LineNumbers)
		sanitizedFileEdits = append(sanitizedFileEdits, f)
	}

	return sanitizedFileEdits
}

func sendAndNotifyChange(redisClient *redis.Client, jsonBody []byte) {
	redisClient.HSet("mergewarnDiffs", *currentUser, string(jsonBody))
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
		if user != *currentUser {
			incomingFileEdits := []FileEdit{}
			json.Unmarshal([]byte(diffSet), &incomingFileEdits)

			// iterate through each file diff and create a notice if that user is editing that line. Oh no!

			for _, fileEdit := range incomingFileEdits {
				for _, localFileEdit := range localFileEdits {
					localFileEdit.User = user

					// also check for line number collision here
					if localFileEdit.Filename == fileEdit.Filename {
						shouldAdd := false

						for _, localLineNumber := range localFileEdit.LineNumbers {
							for _, remoteLineNumber := range fileEdit.LineNumbers {
								if localLineNumber == remoteLineNumber {
									shouldAdd = true
								}
							}
						}

						if shouldAdd {
							conflictFileEdits = append(conflictFileEdits, localFileEdit)
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

	fmt.Print("INCOMING|")
	fmt.Print(time.Now())
	fmt.Print("|")
	fmt.Print(string(jsonBody))
	fmt.Print("\n")
}

func waitForServerChanges(redisClient *redis.Client) {
	var oldConflicts []FileEdit
	pubsub, err := redisClient.Subscribe("newChange")
	if err != nil {
		panic("ERROR: Cannot connect to redis server. Make sure it is running at " + *redisURI)
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

			if len(fetchedConflicts) > 0 || (len(fetchedConflicts) == 0 && len(oldConflicts) > 0) {
				outputConflicts(fetchedConflicts)
			}

			oldConflicts = fetchedConflicts
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

	redisClient := redis.NewClient(&redis.Options{
		Addr:     *redisURI,
		Password: *redisPassword,
		DB:       0, // use default DB
	})

	flag.Parse()

	go waitForServerChanges(redisClient)
	waitForLocalChanges(redisClient)
}
