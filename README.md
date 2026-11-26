# sciget

Install and run containerised scientific software, on a workstation or an HPC
login node. sciget reads a catalog of published containers, fetches each
container's image, and runs it through Apptainer. One static binary, no root.

> **Status: not yet released.** sciget runs containers on a CVMFS share or
> downloaded with `sciget pull` from Nectar or S3. Downloads from the
> Quay and GHCR registries, and modules, are planned.

## Building

```sh
CGO_ENABLED=0 go build   # a static binary
go test ./...
```

## First run

sciget starts with no catalogs. Add one, saying where its containers are,
then download it:

```sh
sciget catalog add neurodesk https://apps-catalog.scigetorg.workers.dev/domains.json \
    --dir /cvmfs/neurodesk.ardc.edu.au/containers
sciget refresh
sciget
```

That link is a domains file, so it adds one catalog per domain:
`neurodesk-neuroimaging` and `neurodesk-microscopy`.

Run on a terminal with no command, sciget opens a full-screen interface:
search, pick a container and open a shell in it or launch its applications.
Mouse and keyboard both work.

Catalog URLs must use https. Plain http is allowed only to this machine,
such as a port forwarded through an SSH tunnel
(`http://localhost:8080/catalog.json`). A local file works too
(`sciget catalog add mylab ./catalog.json`). sciget uses the standard
`HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` variables.

## Catalog files

A catalog is a JSON file saying where its apps list is and which mirrors
serve its containers. The file's name does not matter.

```json
{
  "schemaVersion": 1,
  "title": "Neuroimaging",
  "description": "Neuroimaging tools and resources for research and analysis.",
  "appJsonUrl": "apps.json",
  "downloadUrls": [
    "https://object-store.rc.nectar.org.au/v1/AUTH_dead991e1fa847e3afcca2d3a7041f5d/neurodesk/{file}",
    "https://neurocontainers.s3.us-east-2.amazonaws.com/{file}"
  ]
}
```

| Field | Required | Meaning |
|---|---|---|
| `schemaVersion` | yes | 1 |
| `appJsonUrl` | no | The apps list: a URL, or relative to this file. Without it (and without `eessiCatalogUrl`) the catalog is empty until a later `sciget refresh` finds one |
| `eessiCatalogUrl` | no | Sibling curated EESSI CVMFS Lmod module catalog (`eessi.json`), refreshed alongside `apps.json` and browsed explicitly via `sciget eessi` |
| `eessi_stacks` | no | EESSI stack versions (`YYYY.MM`) enabled for the domain |
| `downloadUrls` | no | Mirrors for the containers, tried in order; each https with `{file}` once. Without them, containers come only from a share (`--dir`) |
| `title`, `description` | no | Shown in a later release |

Other keys are ignored. A **domains file** holds several catalogs under
keys, `{"schemaVersion": 1, "domains": {"neuroimaging": {...}, ...}}`;
`sciget catalog add <name> <url>` adds one catalog per key, named
`<name>-<key>`. Running it again adds only keys new to the file.

## Commands

| Command | What it does |
|---|---|
| `sciget` or `sciget ui` | Browse and launch containers in a full-screen interface |
| `sciget catalog [add\|remove\|use\|unuse\|list]` | Manage catalogs; of those in use, the first listing a name wins |
| `sciget refresh` | Download the latest catalogs in use |
| `sciget find [words...]` | List the containers in the catalogs in use (`--all` includes hidden and other-architecture ones) |
| `sciget info <tool/version>` | Show a container's details and where it is |
| `sciget eessi [find\|info\|launch]` | Browse and launch curated EESSI CVMFS modules (explicit opt-in; no DOI). Put `launch` flags (`--stack`, `--gpu`, `--launcher`) before the tool name |
| `sciget pull <tool/version>...` | Download containers into `container_dir`, `parallel_pulls` (3) at a time |
| `sciget install <tool/version>...` | Download containers, then make a module for each, so `module load` works |
| `sciget doctor` | Report problems, such as stopped downloads, and the commands that fix them |
| `sciget config [get\|set\|unset\|list]` | Show and change settings |
| `--system` (on `catalog add/remove/use/unuse` and `config set/unset`) | Change the system files in `/etc/sciget` for every user; needs root |
| `sciget completion <shell>` | Print a shell completion script |
| `sciget version` | Print the version |

## Pulling containers

`sciget pull fsl/6.0.7.8` downloads a container that is not already on
the machine (on its catalog's share, or downloaded before) into
`container_dir` (default `~/.local/share/sciget/containers`). It tries each of its catalog's `downloadUrls` in turn. A download counts only when its size matches what the server
reported, its SHA-256 matches the catalog's (when the catalog gives
one), and Apptainer can open it. Set `warn_missing_checksum` to true to be
told when no SHA-256 was published. Before downloading, sciget checks the
disk has room (quotas are not seen).

Running a container that is not on the machine (`sciget apptainer exec`,
`shell`, or a launch in the interface) asks first, since the cause may be
a share that is not mounted: `[y]es, [n]o, [A]lways (yes, saved as the
default)`. Always sets `auto_pull`, after which sciget downloads without
asking. Outside a terminal, with `auto_pull` off, it stops and says how
to get the container. In the interface, downloads show in Activity with a
Cancel button; quitting while one runs asks first, then stops it.

A stopped download starts again from the beginning; a killed one leaves a
`*.incomplete` file, which `sciget doctor` lists. sciget does not delete
containers: to remove one, delete its folder in `container_dir`.

Keep `container_dir` separate from neurocommand's container folder: an
image sciget puts there makes neurocommand skip setting it up.

## Using modules

`sciget install fsl/6.0.7.8` downloads the container if needed, then writes
a Lmod module for it: `<module_dir>/<catalog>/fsl/6.0.7.8.lua`
(`module_dir` defaults to `~/.local/share/sciget/modules`). Each command in
the container gets a small wrapper that runs it through sciget, so it gets
the same Apptainer settings as `sciget apptainer exec`. A container on a
share, such as CVMFS, gets no module from sciget: use the share's own
modules. One that is also in your own `container_dir` gets one (see
Shared machines).

sciget never edits your shell files. If the module folder is not on your
`MODULEPATH`, `install` prints the line to add to `~/.bashrc`, once per
catalog:

```sh
module use ~/.local/share/sciget/modules/neuroimaging
```

Then `module load fsl/6.0.7.8` puts its commands on your PATH. sciget finds
the commands by looking inside the container once, and caches what it
finds. Names in `finder_excludes`, such as `bash` and `sudo`, are never
wrapped from a container's own folders. Wrappers call sciget by the name in
`module_sciget` (default `sciget`, found on PATH). Run `sciget install`
again to rebuild a module.

## System setup

An admin, or an image's Dockerfile, sets sciget up for every user with
`--system`, run as root. It writes `/etc/sciget/config.json` and
`/etc/sciget/catalogs.json`, which sciget reads on every run, under each
user's own files:

```sh
sudo sciget catalog add --system neurodesk https://apps-catalog.scigetorg.workers.dev/domains.json \
    --dir /cvmfs/neurodesk.ardc.edu.au/containers
sudo sciget config set --system display_start_command '["/opt/scidesktop/start_desktop.sh"]'
```

- These are defaults, not rules. A user's own settings win, and their own
  catalogs come before the system's. `sciget config list` and
  `sciget catalog list` show where each comes from.
- A user can switch a system catalog off or on (`sciget catalog unuse` and
  `use`); the system's later changes to it still arrive. They cannot change
  or remove it, or add their own under its name.
- `cache_dir`, `container_dir` and `module_dir` are always each user's
  own: sciget writes into them, so one shared folder would let one user
  change what another runs.
- `sciget doctor` names a user's catalog that hides a system one, and a
  user setting that only repeats the system's (often a copy left by an
  older setup), so later system changes would not reach them.
- Each user downloads catalogs into their own cache with `sciget refresh`.

## Shared machines

On a shared workstation or cluster, the admin keeps containers and modules
in folders only root can write, and every user reads them:

1. As root, set root's own `container_dir` and `module_dir` to shared
   folders, such as `/opt/sciget/containers` and `/opt/sciget/modules`.
2. Add the catalog for everyone with that folder as its share:
   `sudo sciget catalog add --system neuroimaging <url> --dir /opt/sciget/containers`.
3. Install containers as root, with a umask such as 022 so every user can
   read what is written: `sudo sciget install fsl/6.0.7.8`. Each gets its
   module in the shared module folder.
4. Add `/opt/sciget/modules/neuroimaging` to `MODULEPATH` for everyone,
   for example in `/etc/profile.d/`.
5. Keep the shared folders current with a scheduled job as root, such as
   `sciget refresh && sciget install fsl/6.0.7.8 …`.

Users then run the shared containers and load the shared modules with no
copies. A container the admin has not provided is downloaded into the
user's own `container_dir`, and `sciget install` writes its module into
their own `module_dir`.

## Licence

MIT. See [LICENSE](LICENSE).
