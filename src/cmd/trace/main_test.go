// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "testing"

func TestListenAddr(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		wantAddr string
		wantErr  bool
	}{
		{
			name:     "empty host",
			addr:     ":8080",
			wantAddr: "localhost:8080",
		},
		{
			name:     "with host",
			addr:     "localhost:8080",
			wantAddr: "localhost:8080",
		},
		{
			name:     "with IP",
			addr:     "127.0.0.1:8080",
			wantAddr: "127.0.0.1:8080",
		},
		{
			name:     "unspecified host",
			addr:     "0.0.0.0:8080",
			wantAddr: "0.0.0.0:8080",
		},
		{
			name:    "host only",
			addr:    "127.0.0.1",
			wantErr: true,
		},
		{
			name:    "port only",
			addr:    "8080",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := listenAddr(tt.addr)
			if tt.wantErr && err == nil {
				t.Errorf("listenAddr(%q) got nil err want non-nil", tt.addr)
			} else if !tt.wantErr && err != nil {
				t.Errorf("listenAddr(%q) got err %v want nil", tt.addr, err)
			} else if got != tt.wantAddr {
				t.Errorf("listenAddr(%q) = %q, want %q", tt.addr, got, tt.wantAddr)
			}
		})
	}
}
