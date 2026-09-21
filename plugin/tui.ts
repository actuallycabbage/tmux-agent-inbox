import { Plugin } from "@opencode/plugin/tui"
import { randomUUID } from "node:crypto"
import { rm } from "node:fs/promises"
import { join } from "node:path"
import { readJSON, serverKey, socketFromEnvironment, stateDirectory, writeJSON } from "./state.ts"

type FocusRequest = {
  id: string
  server: string
  sessionID: string
  created: number
}

export default Plugin.define({
  id: "tmux-agent-inbox",
  async setup(context) {
    const socket = socketFromEnvironment()
    const pane = process.env.TMUX_PANE
    if (!socket || !pane) return

    const directory = stateDirectory(socket)
    const token = randomUUID()
    const file = join(directory, "bridges", `${token}.json`)
    const command = join(directory, `focus-${token}.json`)
    const acknowledgement = join(directory, `ack-${token}.json`)
    let stopped = false
    let server = serverKey(await context.client.server.info())
    let lastHeartbeat = 0
    let lastSessions = ""

    // This runs in each CLI, where TMUX_PANE describes the actual client.
    // The shared server's environment cannot identify which pane owns a tab.
    async function tick() {
      if (stopped) return
      try {
        const request = await readJSON<FocusRequest>(command)
        if (request) {
          await rm(command, { force: true })
          try {
            if (request.server !== server || Date.now() - request.created > 10_000 || !/^ses[\w]+$/.test(request.sessionID)) {
              throw new Error("This session selection has expired; reopen the inbox and try again")
            }
            await context.data.session.sync(request.sessionID)
            const root = context.data.session.root(request.sessionID)
            context.ui.tabs.focus(root)
            context.ui.router.navigate({ type: "session", sessionID: request.sessionID })
            await writeJSON(acknowledgement, { id: request.id })
          } catch (error) {
            await writeJSON(acknowledgement, { id: request.id, error: String(error) })
          }
        }

        const route = context.ui.router.current()
        const current = route.type === "session" ? route.sessionID : undefined
        const sessions = [...new Set([
          ...context.ui.tabs.list().map((tab) => tab.sessionID),
          ...(current ? [current] : []),
        ])]
        const signature = JSON.stringify([current, sessions])
        if (signature !== lastSessions || Date.now() - lastHeartbeat >= 5_000) {
          // Refresh the server identity after daemon restarts as well as the
          // lease. Expired leases prevent jumps into panes whose CLI exited.
          server = serverKey(await context.client.server.info())
          if (stopped) return
          await writeJSON(file, { token, pid: process.pid, pane, server, sessions, current, updated: Date.now() })
          lastSessions = signature
          lastHeartbeat = Date.now()
        }
      } catch (error) {
        console.error("tmux-agent-inbox:", error)
      }
      if (!stopped) timer = setTimeout(tick, 500)
    }

    let timer = setTimeout(tick, 0)
    return async () => {
      stopped = true
      clearTimeout(timer)
      await Promise.all([file, command, acknowledgement].map((path) => rm(path, { force: true })))
    }
  },
})
