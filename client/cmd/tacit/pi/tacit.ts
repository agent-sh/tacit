/**
 * tacit for pi: the reviewer's questions for the owner in the session they
 * are in (written by `tacit install pi`; pi's facts come from tacit's MCP
 * server).
 *
 * Before each agent run it calls `tacit hook questions` with the prompt, the
 * session's id and its file. tacit reads the owner's answer to what this
 * session was asked from the session file (their own turns only), and may
 * return one question about an agent's note the reviewer was unsure of, when
 * it belongs here (same repository, folder or topic). The question goes to
 * the agent as a hidden message, to be asked verbatim. Nothing is printed
 * when no question waits; any failure is silent.
 */
import { spawnSync } from "node:child_process";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const TACIT = process.env.TACIT_BIN || @TACIT_BIN@;

export default function (pi: ExtensionAPI) {
	pi.on("before_agent_start", async (event, ctx) => {
		if (process.env.TACIT_MODEL_JOB === "1") return;
		const payload = JSON.stringify({
			session_id: ctx.sessionManager.getSessionId(),
			transcript_path: ctx.sessionManager.getSessionFile() ?? "",
			cwd: ctx.cwd,
			prompt: event.prompt ?? "",
		});
		let out = "";
		try {
			const r = spawnSync(TACIT, ["hook", "questions"], { input: payload, encoding: "utf-8", timeout: 2000 });
			out = r.status === 0 && typeof r.stdout === "string" ? r.stdout.trim() : "";
		} catch {
			return;
		}
		if (!out) return;
		return { message: { customType: "tacit-question", content: out, display: false } };
	});
}
