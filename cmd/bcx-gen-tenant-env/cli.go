package main

import (
	"errors"
	"fmt"
	"strings"
)

const prog = "bcx-gen-tenant-env"

const usage = "usage: " + prog + " [-h] --out OUT [--check] [--no-incus-check] tenants_yml\n"

const help = usage + `
Generate the broker layer's config from bartixcloud/infra/tenants.yml.

positional arguments:
  tenants_yml       tenants.yml to render

options:
  -h, --help        show this help message and exit
  --out OUT         directory to write the files into
  --check           do not write; exit 1 if any file differs from what would be generated
  --no-incus-check  skip the incus_project existence rule (no Incus, or before Terraform)

exit status: 0 ok, 1 --check found drift, 2 invalid input or refused to run
`

// cliArgs is the parsed command line.
type cliArgs struct {
	tenantsYml   string
	out          string
	check        bool
	noIncusCheck bool
}

// errHelp is returned by parseArgs when -h/--help was given.
var errHelp = errors.New("help requested")

// parseArgs is as strict as the Python's argparse contract: exactly one
// positional, a required --out (also --out=DIR), and nothing it does not know.
// A mistyped --check must never fall through to a write run. Unlike argparse it
// does not accept abbreviated long options for the same reason.
func parseArgs(argv []string) (cliArgs, error) {
	var a cliArgs
	var positionals, unrecognized []string
	outSeen := false
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			positionals = append(positionals, argv[i+1:]...)
			i = len(argv)
		case arg == "-h" || arg == "--help":
			return cliArgs{}, errHelp
		case arg == "--check":
			a.check = true
		case arg == "--no-incus-check":
			a.noIncusCheck = true
		case arg == "--out":
			// A following option-looking word is not a value, as in argparse.
			if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "-") {
				return cliArgs{}, errors.New("argument --out: expected one argument")
			}
			i++
			a.out, outSeen = argv[i], true
		case strings.HasPrefix(arg, "--out="):
			a.out, outSeen = strings.TrimPrefix(arg, "--out="), true
		case strings.HasPrefix(arg, "-") && arg != "-":
			unrecognized = append(unrecognized, arg)
		default:
			positionals = append(positionals, arg)
		}
	}

	var missing []string
	if len(positionals) == 0 {
		missing = append(missing, "tenants_yml")
	}
	if !outSeen {
		missing = append(missing, "--out")
	}
	if len(missing) > 0 {
		return cliArgs{}, fmt.Errorf("the following arguments are required: %s", strings.Join(missing, ", "))
	}
	if len(positionals) > 1 {
		unrecognized = append(unrecognized, positionals[1:]...)
	}
	if len(unrecognized) > 0 {
		return cliArgs{}, fmt.Errorf("unrecognized arguments: %s", strings.Join(unrecognized, " "))
	}
	if a.out == "" {
		return cliArgs{}, errors.New("argument --out: expected a directory, got an empty string")
	}
	a.tenantsYml = positionals[0]
	return a, nil
}
