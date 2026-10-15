import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

// A separate process patches the boundary immediately before the actual helper
// invocation. Go tests inject swaps inside the helper too; these cases prove the
// CLI/library engine paths pass their original scope root, including reporting.
const compiledRoot = fileURLToPath(new URL("../src/", import.meta.url));
const helpers = fileURLToPath(new URL("./helpers.js", import.meta.url));

const scenario = String.raw`
import assert from 'node:assert/strict';
import fs from 'node:fs';
import cp from 'node:child_process';
import {syncBuiltinESMExports} from 'node:module';
import {pathToFileURL} from 'node:url';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
const [source, helpers, action] = process.argv.slice(1);
const base=fs.realpathSync(fs.mkdtempSync(join(tmpdir(),'anchored-route-')));
const root=join(base,'home'), outside=join(base,'outside');
fs.mkdirSync(root); fs.mkdirSync(outside);
let swapped=false, expectedRoot=root, relativeDir='.claude';
const load=(name)=>import(pathToFileURL(join(source,name+'.js')).href);
try {
 if(action==='git-exclude') {
  cp.execFileSync('git',['-C',root,'init','-q']);
  expectedRoot=join(root,'.git');relativeDir='info';
 } else {
  if(action.startsWith('cursor')) relativeDir='.cursor';
  if(action.startsWith('codex')) relativeDir='.codex';
  fs.mkdirSync(join(root,relativeDir));
 }
 const settings=relativeDir==='.claude'?'settings.json':'hooks.json';
 for(const dir of [join(expectedRoot,relativeDir),outside]) {
  fs.writeFileSync(join(dir,'CLAUDE.md'),'sentinel');
  fs.writeFileSync(join(dir,'exclude'),'# engineer ignore\n');
  fs.writeFileSync(join(dir,settings),JSON.stringify({hooks:{PreToolUse:[{matcher:'Bash',hooks:[{type:'command',command:'guard'}]}]}}));
  fs.mkdirSync(join(dir,'empty'));
 }
 const original=cp.spawnSync;
 cp.spawnSync=function(command,args,options) {
  if(String(command).endsWith('/native/fs-helper')) {
   const q=JSON.parse(options.input);
   if(!swapped && q.root===expectedRoot && q.path.startsWith(relativeDir+'/')) {
    swapped=true;
    fs.renameSync(join(expectedRoot,relativeDir),join(expectedRoot,'original'));
    fs.symlinkSync(outside,join(expectedRoot,relativeDir));
   }
  }
  return original.call(this,command,args,options);
 };
 syncBuiltinESMExports();
 const path=join(root,relativeDir,'CLAUDE.md');
 const ops=await load('engine/fsops');
 if(action==='write') assert.throws(()=>ops.writeAtomic(path,Buffer.from('replacement'),{root}));
 else if(action==='hash') assert.equal((await load('engine/converge')).sha256File(path,root),null);
 else if(action==='report') {
  const {testContext}=await import(pathToFileURL(helpers).href);
  const ctx=testContext(base);ctx.paths.home=root;
  ctx.state.snapshot=[{artifactId:'x',ideType:'IDE_TYPE_CLAUDE_CODE',type:'ARTIFACT_TYPE_CLAUDE_MD',scope:'ARTIFACT_SCOPE_USER_GLOBAL',name:'test',installPath:'.claude/CLAUDE.md',primaryHash:'x',placedHash:'x',isHook:false,placedPaths:['.claude/CLAUDE.md']}];
  assert.deepEqual((await load('agent')).buildInstalledReport(ctx),[]);
 } else if(action==='remove') assert.equal(ops.removePlacedFile(root,'.claude/CLAUDE.md'),false);
 else if(action==='rmdir') ops.removeEmptyCreatedDirs(root,['.claude/empty'],1);
 else if(action==='git-exclude') assert.equal(await (await load('engine/gitGate')).ensureGitExcluded(root,['new-managed-file']),false);
 else {
  const activation=await load('engine/hookActivation');
  const ide=action.startsWith('cursor')?'IDE_TYPE_CURSOR':action.startsWith('codex')?'IDE_TYPE_CODEX':'IDE_TYPE_CLAUDE_CODE';
  const method=action.endsWith('remove')?activation.deactivateHook:activation.activateHook;
  const result=method(ide,root,relativeDir+'/hooks/guard.sh',{event:'PreToolUse',matcher:'Bash',command:'guard'},'user');
  assert.equal(result.ok,false);
 }
 assert.equal(swapped,true,'test must reach helper with original scope root');
 assert.equal(fs.readFileSync(join(outside,'CLAUDE.md'),'utf8'),'sentinel');
 assert.equal(fs.readFileSync(join(outside,'exclude'),'utf8'),'# engineer ignore\n');
 assert.equal(fs.readFileSync(join(outside,settings),'utf8'),JSON.stringify({hooks:{PreToolUse:[{matcher:'Bash',hooks:[{type:'command',command:'guard'}]}]}}));
 assert.equal(fs.existsSync(join(outside,'empty')),true);
 assert.equal(fs.readdirSync(outside).some(n=>n.startsWith('.harness-tmp-')),false);
 console.log('protected '+action);
} finally { fs.rmSync(base,{recursive:true,force:true}); }
`;

describe("anchored filesystem engine routes", () => {
  for (const action of ["write", "hash", "report", "remove", "rmdir", "claude-activate", "claude-remove", "codex-activate", "codex-remove", "cursor-activate", "cursor-remove", "git-exclude"]) {
    it(`a parent swap before ${action} cannot reach outside data`, () => {
      const output = execFileSync(process.execPath, ["--input-type=module", "-e", scenario, compiledRoot, helpers, action], { encoding: "utf8" });
      assert.ok(output.includes(`protected ${action}`));
    });
  }
});


it("a missing packaged helper fails closed without a pathname fallback", () => {
  const script = String.raw`
    import fs from 'node:fs';
    import assert from 'node:assert/strict';
    import {syncBuiltinESMExports} from 'node:module';
    import {pathToFileURL} from 'node:url';
    import {join} from 'node:path';
    import {tmpdir} from 'node:os';
    const root=fs.mkdtempSync(join(tmpdir(),'helper-missing-'));
    const exists=fs.existsSync;
    fs.existsSync=(p)=>String(p).endsWith('/native/fs-helper')?false:exists(p);
    syncBuiltinESMExports();
    const {writeAtomic}=await import(pathToFileURL(join(process.argv[1],'engine/fsops.js')).href);
    try {
      assert.throws(()=>writeAtomic(join(root,'file'),Buffer.from('x'),{root}),/filesystem helper is missing/);
      assert.deepEqual(fs.readdirSync(root),[]);
    } finally { fs.rmSync(root,{recursive:true,force:true}); }
  `;
  execFileSync(process.execPath, ["--input-type=module", "-e", script, compiledRoot], { encoding: "utf8" });
});
