import React, { useEffect, useId, useRef, useState } from "react";
import { activeName, insertName } from "./name-search.js";

export function NamePicker({ label, kind, value, onChange, api, disabled }) {
 const id=useId(), input=useRef(null), search=useRef(api);
 search.current=api;
 const [focused,setFocused]=useState(false), [caret,setCaret]=useState(value.length);
 const [result,setResult]=useState({query:"",names:[]}), [status,setStatus]=useState(""), [active,setActive]=useState(-1), [dismissed,setDismissed]=useState(false);
 const token=activeName(value,caret), query=token.query;
 const options=result.query===query?result.names.filter(name=>!token.selected.includes(name)):[];
 const open=focused&&!disabled&&!dismissed&&query.length>0;
 useEffect(()=>{
  let cancelled=false;
  setActive(-1);setResult({query:"",names:[]});setStatus("");
  if(!focused||disabled||dismissed||!query)return;
  if(!/^[a-z0-9-]{1,40}$/.test(query)){setStatus("Type a username or group name.");return;}
  setStatus("Searching…");
  const timer=setTimeout(async()=>{
   try {
    const found=await search.current(`directory/${kind}?q=${encodeURIComponent(query)}`);
    if(!cancelled){setResult({query,names:found});setStatus("");}
   } catch(e) { if(!cancelled)setStatus("Search unavailable. You can still enter names manually."); }
  },200);
  return()=>{cancelled=true;clearTimeout(timer);};
 },[query,kind,focused,disabled,dismissed]);
 function select(name) {
  const next=insertName(value,token,name);
  onChange(next.value);setCaret(next.caret);setActive(-1);
  input.current.focus();
  requestAnimationFrame(()=>input.current?.setSelectionRange(next.caret,next.caret));
 }
 function keydown(e) {
  if(e.key==="Escape"){setDismissed(true);setActive(-1);return;}
  if((e.key==="ArrowDown"||e.key==="ArrowUp")&&open&&options.length){e.preventDefault();setActive(index=>e.key==="ArrowDown"?(index+1)%options.length:(index<=0?options.length-1:index-1));}
  if(e.key==="Enter"&&open){e.preventDefault();if(options.length)select(options[active>=0&&active<options.length?active:0]);}
 }
 return <div className="name-picker">
  <label htmlFor={id}>{label}</label>
  <input ref={input} id={id} role="combobox" autoComplete="off" spellCheck={false} disabled={disabled} value={value} placeholder={kind==="users"?"Search users…":"Search groups…"}
   aria-autocomplete="list" aria-expanded={open&&options.length>0} aria-controls={`${id}-options`} aria-describedby={`${id}-help`} aria-activedescendant={open&&active>=0&&active<options.length?`${id}-option-${active}`:undefined}
   onFocus={()=>{setFocused(true);setDismissed(false);setCaret(input.current.selectionStart);}} onBlur={()=>{setFocused(false);setActive(-1);}}
   onChange={e=>{onChange(e.target.value);setCaret(e.target.selectionStart);setDismissed(false);}}
   onSelect={e=>setCaret(e.target.selectionStart)} onKeyDown={keydown}/>
  <ul className="name-suggestions" id={`${id}-options`} role="listbox" aria-label={`${label} suggestions`} hidden={!open||options.length===0}>
   {options.map((name,index)=><li key={name} id={`${id}-option-${index}`} role="option" aria-selected={index===active} onPointerDown={e=>e.preventDefault()} onClick={()=>select(name)}>{name}</li>)}
  </ul>
  <p id={`${id}-help`} className="intro-copy" role="status">{open?(status||(!options.length?"No matching names.":"Select a name, or use ↑ / ↓ and Enter.")):"Type to search; separate multiple names with commas."}</p>
 </div>;
}
