import { createHash } from "node:crypto"
import { mkdir, readFile, rename, writeFile } from "node:fs/promises"
import { homedir } from "node:os"
import { join } from "node:path"

export function stateDirectory(socket: string) {
  const base = process.env.XDG_STATE_HOME || join(homedir(), ".local", "state")
  // Pane IDs are unique within a tmux server, not across tmux sockets.
  // Keep the legacy state path in sync with Go across project renames.
  return join(base, "tmux-opencode-notify", createHash("sha256").update(socket).digest("hex").slice(0, 16))
}

export function socketFromEnvironment() {
  return process.env.TMUX?.replace(/,\d+,\d+$/, "")
}

export async function readJSON<T>(file: string): Promise<T | undefined> {
  try {
    return JSON.parse(await readFile(file, "utf8")) as T
  } catch (error) {
    if (error instanceof Error && "code" in error && error.code === "ENOENT") return undefined
    throw error
  }
}

export async function writeJSON(file: string, value: unknown) {
  await mkdir(join(file, ".."), { recursive: true, mode: 0o700 })
  const temporary = `${file}.${process.pid}.tmp`
  await writeFile(temporary, JSON.stringify(value), { mode: 0o600 })
  await rename(temporary, file)
}

export function serverKey(info: { pid: number; urls: readonly string[] }) {
  return JSON.stringify([info.pid, [...info.urls].sort()])
}
