import assert from "node:assert/strict";
import test from "node:test";
import { activeName, insertName } from "./name-search.js";

test("selecting a second user preserves earlier selections and starts the next search",()=>{
 const value="alice, bo", token=activeName(value);
 assert.equal(token.query,"bo");
 assert.deepEqual(token.selected,["alice"]);
 const next=insertName(value,token,"bob");
 assert.deepEqual(next,{value:"alice, bob, ",caret:12});
 assert.equal(activeName(next.value,next.caret).query,"");
});

test("editing a name in the middle preserves later selections",()=>{
 const value="alice, te, bob", token=activeName(value,9);
 assert.equal(token.query,"te");
 assert.deepEqual(token.selected,["alice","bob"]);
 assert.deepEqual(insertName(value,token,"team"),{value:"alice, team, bob",caret:11});
});

test("searching inside a token uses its entire name and normalizes case",()=>{
 const token=activeName("alice, TEAM, bob",9);
 assert.equal(token.query,"team");
 assert.deepEqual(token.selected,["alice","bob"]);
});

test("selecting a mixed-case result preserves its saved spelling",()=>{
 const value="Alice, mi", token=activeName(value);
 assert.deepEqual(token.selected,["Alice"]);
 assert.deepEqual(insertName(value,token,"Mixed-Team"),{value:"Alice, Mixed-Team, ",caret:19});
});

test("spaces, commas and empty input start a fresh selection",()=>{
 for(const value of ["","alice, ","alice bob "]){assert.equal(activeName(value).query,"");}
 assert.deepEqual(activeName("alice\tb").selected,["alice"]);
 assert.deepEqual(insertName("",activeName(""),"alice"),{value:"alice, ",caret:7});
});
