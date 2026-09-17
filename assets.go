package main

import "embed"

// Assets is everything lathe needs to read about itself: the agent roster and
// the prompts. Compiling them in means there is no install root to resolve and
// no way to end up reading a half-installed copy's prompts.
//
//go:embed lathe.toml prompts
var Assets embed.FS
