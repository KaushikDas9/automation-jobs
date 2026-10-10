// Command job is a stub. Replace it with the real job.
//
// Keep everything for this project inside this folder: its own go.mod, its own
// README.md, its own db-role.sql if it uses a database, and no imports from other
// projects. Secrets are named PROJECT_NAME_SECRET, for example MYPROJ_API_TOKEN.
package main

import (
	"fmt"
	"os"
)

func main() {
	if os.Getenv("MYPROJ_API_TOKEN") == "" {
		// Not configured yet. Exit 0 so the stub does not fail a scheduled run.
		fmt.Println("template job: not configured, nothing to do")
		return
	}
	fmt.Println("template job: replace this stub with the real work")
}
