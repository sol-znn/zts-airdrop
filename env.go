package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// envPrefix namespaces every setting this tool reads from the environment, so
// a .env shared with other tools cannot quietly redefine, say, NODE.
const envPrefix = "ZTS_AIRDROP_"

// LoadDotEnv reads KEY=VALUE lines from path into the process environment.
//
// Values already present in the real environment win. That ordering is the
// one people expect from a .env — the file is the project's defaults, the
// shell is the override for this one invocation — and it is also what makes
// `ZTS_AIRDROP_AMOUNT=1 zts-airdrop -confirm` do the obvious thing.
//
// A missing file is not an error: .env is a convenience, and requiring one
// would break every invocation that passes flags directly.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return fmt.Errorf("%s line %d: expected KEY=VALUE", path, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		// Quotes are stripped so a mnemonic can be written as one quoted
		// string; without that, the trailing-comment rule below would eat
		// half of an unquoted value containing a #.
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' ||
			value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}

		if _, present := os.LookupEnv(key); present {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// ApplyEnvDefaults fills in any flag that was not given on the command line
// from ZTS_AIRDROP_<FLAG>, with dashes as underscores: -min-holding reads
// ZTS_AIRDROP_MIN_HOLDING.
//
// It runs after parsing and only touches flags the command line left alone,
// so an explicit flag always beats the file — including an explicit `-x=false`
// against a true in .env, which a pre-parse default could not express.
func ApplyEnvDefaults(set *flag.FlagSet) error {
	explicit := map[string]bool{}
	set.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	var firstErr error
	set.VisitAll(func(f *flag.Flag) {
		if firstErr != nil || explicit[f.Name] {
			return
		}
		key := envPrefix + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		value, ok := os.LookupEnv(key)
		if !ok || value == "" {
			return
		}
		if err := f.Value.Set(value); err != nil {
			firstErr = fmt.Errorf("%s=%q: %w", key, value, err)
		}
	})
	return firstErr
}
