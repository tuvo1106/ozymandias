// Builds dist/esm and dist/cjs from src/ with plain tsc (no bundler: the SDK
// has no dependencies to bundle and keeping one file per module keeps stack
// traces readable in the host app).
import { execFileSync } from "node:child_process";
import { mkdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const tsc = join(root, "node_modules", "typescript", "bin", "tsc");
const run = (project) => execFileSync(process.execPath, [tsc, "-p", project], { cwd: root, stdio: "inherit" });

rmSync(join(root, "dist"), { recursive: true, force: true });
run("tsconfig.esm.json");
run("tsconfig.cjs.json");
mkdirSync(join(root, "dist", "cjs"), { recursive: true });
writeFileSync(join(root, "dist", "cjs", "package.json"), JSON.stringify({ type: "commonjs" }) + "\n");
