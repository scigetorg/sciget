# sciget

Install and launch containerised scientific software, on a workstation or an HPC login node.

sciget fetches published containers, installs them as Lmod modules, and generates desktop menu
entries that reach an application by exactly the same route the terminal does. One static binary,
no interpreter, no root.

> **Status: pre-release.** v0 reproduces the existing neurocommand menu generation from the same
> input, so it can replace it in place while the upstream pipeline is migrated separately. It runs
> inside scidesktop rather than being typed by hand. Not yet an official release.

## Building

```sh
go build ./...
go test ./...
```

No dependencies beyond the standard library.

## Using

```sh
sciget list -json apps.json    # print every entry in the catalogue
sciget version
```

## Layout

Four files, one package, read top to bottom.

| File | Contents |
|---|---|
| `main.go` | Flags, subcommand switch, and the `list` command. |
| `catalogue.go` | What an entry is, and how the catalogue is read. |
| `catalogue_test.go` | Behaviour of the above, against `testdata/apps.json`. |
| `corpus_test.go` | The same derivation checked against a real neurocommand checkout. |

## Testing against the real catalogue

The unit tests use a small fixture so the repository stays self-contained. The corpus test checks
the derivation against every published container, and skips unless pointed at a checkout:

```sh
SCIGET_APPSJSON=~/neurocommand/neurodesk/apps.json \
SCIGET_LOGTXT=~/neurocommand/cvmfs/log.txt \
go test -run Corpus -v
```

It asserts that every command-line entry derives a distinct container name, and that the derived
names and the published list agree in both directions.

## Design

Two things are worth knowing before reading the code:

- **A container is addressable from catalogue data alone.** `<tool>_<version>_<builddate>` gives the
  CVMFS path and the registry reference, with no lookup table anywhere. The corpus test proves it
  against the full published set.
- **Lmod is the single access path.** Nothing bypasses `module load`, so a menu click and a terminal
  session resolve to the identical wrapper.

## Licence

MIT. See [LICENSE](LICENSE).
