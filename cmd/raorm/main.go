// Command raorm is a STUB, and says so when you run it.
//
// The commands need your models, and a binary installed from this repository
// cannot see them — so the real tool is five lines in your own module:
//
//	package main
//
//	import (
//		"github.com/gsoultan/raorm/tool"
//		"example.com/app/model"
//	)
//
//	func main() { tool.Main(model.All(), model.Queries()) }
//
// This binary exists so `go install github.com/gsoultan/raorm/cmd/raorm@latest`
// fails with that instruction rather than with nothing.
package main

import "github.com/gsoultan/raorm/tool"

func main() { tool.Main(nil, nil) }
