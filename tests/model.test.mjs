// Tests for Model.js (forked from sbdbar). Model.js is a QML `.pragma library`
// script (no ES exports), so load it into a vm context instead of importing it.
import { readFileSync } from "node:fs"
import { test } from "node:test"
import assert from "node:assert/strict"
import vm from "node:vm"

const src = readFileSync(new URL("../Model.js", import.meta.url), "utf8").replace(/^\.pragma library\s*$/m, "")
const Model = vm.createContext({})
vm.runInContext(src, Model)

const palette = { accent: "accent", foreground: "fg", urgent: "urgent", faint: "faint" }

test("delay: honk-agent's -1 means measured unavailable", () => {
  assert.equal(Model.delayText(-1), "timeout")
  assert.equal(Model.delayColor(-1, palette), "urgent")
})

test("delay: 0 is untested, positive values are bucketed", () => {
  assert.equal(Model.delayText(0), "")
  assert.equal(Model.delayColor(0, palette), "faint")
  assert.equal(Model.delayText(75), "75ms")
  assert.equal(Model.delayColor(75, palette), "accent")
  assert.equal(Model.delayColor(300, palette), "fg")
  assert.equal(Model.delayColor(1500, palette), "urgent")
})

test("formatRate and formatBytes", () => {
  assert.equal(Model.formatBytes(0), "0 B")
  assert.equal(Model.formatBytes(1536), "1.5 KB")
  assert.match(Model.formatRate(1536), /^1\.5 KB\/s$/)
})

test("normalizeMode accepts honk-agent's Clash mode names", () => {
  assert.equal(Model.normalizeMode("Global"), "Global")
  assert.equal(Model.normalizeMode("direct"), "Direct")
  assert.equal(Model.normalizeMode(""), "Rule")
})
