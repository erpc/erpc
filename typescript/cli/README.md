# eRPC CLI

This package contains a simple js wrapper arround the eRPC binary.

## Usage

To use this package, you can directly execute it via `npx` or `bunx`:

```bash
npx @erpc-cloud/cli
```

This will run eRPC with default config.

And create a `erpc.ts` file:

## Commands

Every commands will run using the config:
 - a `--config` flag (e.g. `npx @erpc-cloud/cli --config erpc.ts`)
 - a config file placed as the first arg of the cmd (e.g. `npx @erpc-cloud/cli erpc.ts`)
 - searching a default config `erpc.ts` / `erpc.js` / `erpc.yaml` / `erpc.yml` in the current directory

### start

Will run eRPC with the config.

```bash
npx @erpc-cloud/cli start
```

Same as running `npx @erpc-cloud/cli` without any args.


### validate

Validate a config file.

```bash
npx @erpc-cloud/cli validate
```