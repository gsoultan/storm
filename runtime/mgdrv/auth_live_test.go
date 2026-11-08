// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgdrv_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime/mgdrv"
)

// SCRAM-SHA-256 against a server with authentication on, named by
// STORM_MONGO_AUTH_URI — a standalone is enough:
//
//	docker run -d -p 27018:27017 -e MONGO_INITDB_ROOT_USERNAME=storm \
//	  -e MONGO_INITDB_ROOT_PASSWORD=secret mongo:7
//	STORM_MONGO_AUTH_URI='mongodb://storm:secret@localhost:27018/storm_test?authSource=admin'
func TestLiveSCRAM(t *testing.T) {
	uri := os.Getenv("STORM_MONGO_AUTH_URI")
	if uri == "" {
		t.Skip("STORM_MONGO_AUTH_URI is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := mgdrv.OpenURI(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Exec(ctx, `{"ping":1}`, nil); err != nil {
		t.Fatal(err)
	}

	cfg, err := mgdrv.ParseURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password += "x"
	if _, err := mgdrv.Open(ctx, cfg); err == nil || !strings.Contains(err.Error(), "18") {
		t.Fatalf("a wrong password must fail with AuthenticationFailed (18), got %v", err)
	}
}
