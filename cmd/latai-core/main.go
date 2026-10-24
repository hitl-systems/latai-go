// SPDX-License-Identifier: Apache-2.0 OR MIT

// Command latai-core puts the prebuilt latai core of this SDK version into the user cache,
// checked against the pins the SDK was released with, and prints its folder:
//
//	go run github.com/hitl-systems/latai-go/cmd/latai-core@latest
//
// LATAI_CORE_URL names another place to download from (a mirror); LATAI_CORE_CACHE another cache.
package main

import (
	"context"
	"fmt"
	"os"

	latai "github.com/hitl-systems/latai-go"
)

func main() {
	dir, err := latai.EnsureCore(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(dir)
}
