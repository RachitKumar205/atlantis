package main

import (
	"fmt"
	"strings"
)

// sdkLanguage is the client `tide generate` writes.
type sdkLanguage string

const (
	langGo     sdkLanguage = "go"
	langPython sdkLanguage = "python"
)

// defaultLanguage is what an absent `language:` means.
//
// Go, so every tide.yaml written before Python existed keeps producing the
// same tree. A default of "unset, choose one" would break every caller on
// upgrade for no gain.
const defaultLanguage = langGo

// resolveLanguage reads `language:` from tide.yaml.
//
// The value is compared case-insensitively after trimming, since it arrives
// from a hand-written file and from ATL_LANGUAGE. An unknown one is refused
// and names what is accepted: generating Go for a caller that asked for
// something else would write a whole tree in the wrong language and report
// success.
func resolveLanguage(raw string) (sdkLanguage, error) {
	got := strings.ToLower(strings.TrimSpace(raw))
	switch got {
	case "":
		return defaultLanguage, nil
	case string(langGo):
		return langGo, nil
	case string(langPython):
		return langPython, nil
	}
	return "", fmt.Errorf("language: %q is not a language tide generates. Use %q or %q",
		raw, langGo, langPython)
}

// ownedRoots are the directories generate writes and may therefore remove.
//
// Everything else in output_dir belongs to the caller.
//
// The two languages differ in where buf's output lands. Go's goes to `pb/`,
// its own root. Python's lands inside `atlantis/`, beside the clients that
// import it, because protoc derives a module path from the proto path — so
// `atlantis` covers both there and there is no second root.
func (l sdkLanguage) ownedRoots() []string {
	if l == langPython {
		return []string{"atlantis"}
	}
	return []string{"atlantis", "pb", "client"}
}

// ownedFiles are the files generate writes at the top of output_dir.
func (l sdkLanguage) ownedFiles() []string { return []string{"buf.gen.yaml", "buf.yaml"} }

// needsGoModule reports whether generate must read the caller's go.mod.
//
// Only Go does. The module path becomes the import prefix of every emitted
// file, and Python has no equivalent — reading go.mod anyway makes
// `tide generate` fail in a Python repository before it does anything, with an
// error about a file that repository has no reason to contain.
func (l sdkLanguage) needsGoModule() bool { return l == langGo }
