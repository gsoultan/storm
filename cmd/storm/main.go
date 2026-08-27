// Command storm is a STUB, and says so when you run it.
//
// The commands need your models, and a binary installed from this repository
// cannot see them — so the real tool is five lines in your own module:
//
//	package main
//
//	import (
//		"github.com/gsoultan/storm/tool"
//		"example.com/app/model"
//	)
//
//	func main() { tool.Main(model.All(), model.Queries()) }
//
// This binary exists so `go install github.com/gsoultan/storm/cmd/storm@latest`
// fails with that instruction rather than with nothing.
package main

import "github.com/gsoultan/storm/tool"

func main() { tool.Main(nil, nil) }
