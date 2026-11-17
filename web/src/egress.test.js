import test from "node:test";
import assert from "node:assert/strict";
import { parsePodLabels, egressDestination, egressRuleInput, emptyEgressDraft, egressDraftFromRule, updatedEgressRules } from "./egress.js";

test("pod selectors require explicit unique labels",()=>{
 assert.deepEqual(parsePodLabels(" app.kubernetes.io/name=traefik\n\ncomponent = ingress\nflag="),{"app.kubernetes.io/name":"traefik",component:"ingress",flag:""});
 for(const input of ["", "app", "=traefik", "app=a\napp=b"])assert.throws(()=>parsePodLabels(input));
 assert.equal(Object.getPrototypeOf(parsePodLabels("__proto__=test")),Object.prototype);
 assert.equal(Object.hasOwn(parsePodLabels("__proto__=test"),"__proto__"),true);
});

test("rule inputs never send a workspace selector or mixed destination",()=>{
 const draft={name:"proxy",protocol:"TCP",port:"8443",type:"pods",namespace:"traefik",labels:"app=traefik",cidr:"192.0.2.10"};
 assert.deepEqual(egressRuleInput(draft),{name:"proxy",protocol:"TCP",port:8443,namespace:"traefik",pod_selector:{app:"traefik"}});
 assert.deepEqual(egressRuleInput({...draft,type:"cidr"}),{name:"proxy",protocol:"TCP",port:8443,cidr:"192.0.2.10"});
});

test("destinations show scope and labels",()=>{
 assert.equal(egressDestination({cidr:"192.0.2.10/32"}),"192.0.2.10/32");
 assert.equal(egressDestination({namespace:"traefik",pod_selector:{app:"traefik"}}),"traefik: app=traefik");
});

test("add dialogs start fresh and edit dialogs roundtrip either destination",()=>{
 const first=emptyEgressDraft();first.name="unsaved";
 assert.equal(emptyEgressDraft().name,"");
 for(const rule of [
  {name:"api",cidr:"192.0.2.10/32",protocol:"TCP",port:443},
  {name:"proxy",namespace:"traefik",pod_selector:{"app.kubernetes.io/name":"traefik",component:"ingress"},protocol:"TCP",port:8443},
 ])assert.deepEqual(egressRuleInput(egressDraftFromRule(rule)),rule);
});

test("saving a dialog adds or replaces only the selected rule without mutating the list",()=>{
 const existing={name:"api",cidr:"192.0.2.10/32",protocol:"TCP",port:443};
 const rules=[existing], draft={...emptyEgressDraft(),name:"dns",cidr:"192.0.2.53",protocol:"UDP",port:"53"};
 const added=updatedEgressRules(rules,draft,null);
 assert.equal(added.length,2);assert.equal(rules.length,1);
 const edited=updatedEgressRules(added,{...egressDraftFromRule(existing),name:"renamed",port:"80"},"api");
 assert.equal(edited[0].name,"renamed");assert.equal(edited[0].port,80);
 assert.deepEqual(edited[1],added[1]);assert.equal(existing.port,443);
 assert.throws(()=>updatedEgressRules(rules,egressDraftFromRule(existing),null),/already exists/);
 assert.throws(()=>updatedEgressRules(added,{...draft,name:"api"},"dns"),/already exists/);
 assert.throws(()=>updatedEgressRules([],egressDraftFromRule(existing),"api"),/was removed/);
});
