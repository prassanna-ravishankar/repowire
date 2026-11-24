import { describe, expect, it } from "vitest";
import { fixtures } from "./fixtures";
import { classifyInput, parseTranscript } from "./transcript";

describe("parseTranscript", () => {
  const items = parseTranscript(fixtures(Date.parse("2026-10-25T14:05:00Z")).messages);

  it("renders human, tool, assistant and peer entries in order", () => {
    expect(items.map((i) => i.kind)).toEqual(["human", "tool", "assistant", "peer", "assistant", "human"]);
  });

  it("collapses a tool call with its result, exit code and duration", () => {
    const tool = items[1];
    expect(tool).toMatchObject({ kind: "tool", name: "workspace_exec", exitCode: 1, durationMs: 1800, isError: false });
    expect(tool.kind === "tool" && tool.args).toEqual({ command: "cd project && go test ./parser/..." });
    expect(tool.kind === "tool" && tool.output).toContain("TestParseQuoted");
  });

  it("drops thinking and keeps assistant text", () => {
    expect(items[2]).toMatchObject({ kind: "assistant", text: expect.stringContaining("Three tests fail") });
    expect(JSON.stringify(items)).not.toContain("run tests");
  });

  it("marks peer input untrusted with sender and ask id, and links the reply", () => {
    expect(items[3]).toMatchObject({ kind: "peer", from: "@repowire-claude-code", type: "ask", correlationId: "a81f02bc" });
    expect(items[3].kind === "peer" && items[3].text).toMatch(/^Can you check whether foo_test still flakes\?/);
    expect(items[4]).toMatchObject({ kind: "assistant", replyTo: "@repowire-claude-code" });
    expect(items[5]).toMatchObject({ kind: "human", text: "And the flaky one?" });
  });

  it("surfaces assistant errors", () => {
    const [item] = parseTranscript([{ kind: "pi.assistant", model: [{ role: "assistant", stopReason: "error", errorMessage: "model error 429", content: [] }] }]);
    expect(item).toMatchObject({ kind: "assistant", text: "", error: "model error 429" });
  });

  it("accepts bare messages, wrapped lists and JSON strings", () => {
    const bare = [{ role: "user", content: [{ type: "text", text: "hello" }], timestamp: 1 }, { role: "assistant", content: [{ type: "text", text: "hi" }], timestamp: 2 }];
    expect(parseTranscript(bare).map((i) => i.kind)).toEqual(["human", "assistant"]);
    expect(parseTranscript({ items: bare })).toHaveLength(2);
    expect(parseTranscript(JSON.stringify(bare))).toHaveLength(2);
    expect(parseTranscript("not json")).toEqual([]);
    expect(parseTranscript(null)).toEqual([]);
  });

  it("shows resets and compactions as notices", () => {
    expect(parseTranscript([{ kind: "pi.reset" }, { kind: "pi.compaction", model: [{ role: "user", content: "summary" }] }]).map((i) => i.kind)).toEqual(["notice", "notice"]);
  });
});

describe("classifyInput", () => {
  it("unescapes peer-message bodies and attributes", () => {
    const input = classifyInput('<peer-message from="@a&amp;b" type="notify">\nx &lt; y &amp;&amp; &quot;z&quot;\n</peer-message>', null);
    expect(input).toMatchObject({ kind: "peer", from: "@a&b", type: "notify", text: 'x < y && "z"', correlationId: null });
  });

  it("recognises ask replies", () => {
    expect(classifyInput("[ack #a81f from @reviewer] looks good", null)).toMatchObject({ kind: "peer", type: "ack", from: "@reviewer", correlationId: "a81f", text: "looks good" });
  });

  it("treats relayed human messages as human", () => {
    expect(classifyInput("@telegram: ship it", null)).toEqual({ kind: "human", text: "ship it", via: "@telegram" });
    expect(classifyInput("@someone-else: not human", null)).toEqual({ kind: "human", text: "@someone-else: not human", via: null });
  });

  it("uses the repowire.message source for unwrapped peer text", () => {
    expect(classifyInput("plain", { from: "@bot", type: "notify", correlationId: null })).toMatchObject({ kind: "peer", from: "@bot", text: "plain" });
    expect(classifyInput("plain", { from: "@dashboard", type: "notify", correlationId: null })).toEqual({ kind: "human", text: "plain", via: "@dashboard" });
  });
});
