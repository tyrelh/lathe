package config

import "testing"

func TestHash(t *testing.T) {
	if Hash("a") == Hash("b") || Hash("a") != Hash("a") || len(Hash("")) != 64 {
		t.Fatal("hash is not a sha256 of the body")
	}
}
