// Generates src/gen from the manifesto protos at the version eRPC's go.mod
// pins, so the library speaks exactly the server's wire.
import { execFileSync } from "node:child_process";
import { readFileSync, rmSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const pkg = join(dirname(fileURLToPath(import.meta.url)), "..");
const root = join(pkg, "..", "..");
const module = "github.com/blockchain-data-standards/manifesto";

const goMod = readFileSync(join(root, "go.mod"), "utf8");
const pin = goMod.match(
  new RegExp(`^\\s*${module.replaceAll(".", "\\.")}\\s+(\\S+)`, "m"),
);
if (!pin) throw new Error(`go.mod names no ${module}`);
const { Dir } = JSON.parse(
  execFileSync("go", ["mod", "download", "-json", `${module}@${pin[1]}`], {
    cwd: root,
    encoding: "utf8",
  }),
);

const common = "env=node,esModuleInterop=true,oneof=unions-value,forceLong=bigint";
// src/gen holds the codecs and client the library runs; src/gen/types the
// same messages as plain interfaces, which the public API names, so a
// consumer's typecheck never needs the bundled protobuf runtime.
const runs = [
  ["gen", `outputServices=grpc-js,outputJsonMethods=false,outputPartialMethods=false,${common}`],
  [join("gen", "types"), `onlyTypes=true,outputServices=false,${common}`],
];
rmSync(join(pkg, "src", "gen"), { recursive: true, force: true });
for (const [dir, opts] of runs) {
  const out = join(pkg, "src", dir);
  mkdirSync(out, { recursive: true });
  execFileSync(
    "protoc",
    [
      `--plugin=protoc-gen-ts_proto=${join(pkg, "node_modules", ".bin", "protoc-gen-ts_proto")}`,
      `--ts_proto_out=${out}`,
      `--ts_proto_opt=${opts}`,
      `-I=${join(Dir, "evm")}`,
      join(Dir, "evm", "stream.proto"),
    ],
    { stdio: "inherit" },
  );
}
console.log(`generated src/gen from ${module}@${pin[1]}`);
