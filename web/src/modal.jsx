import React, { useEffect, useId, useRef } from "react";
import { createPortal } from "react-dom";

// Native HTML dialog supplies focus trapping, an inert background and top-layer
// rendering. This is a fixed overlay in the current page, never a new window.
export function Modal({title,onClose,busy=false,children}) {
 const dialogRef=useRef(null), titleId=useId();
 const controlsRef=useRef({busy,onClose});
 controlsRef.current={busy,onClose};
 useEffect(()=>{
  const dialog=dialogRef.current, previousFocus=document.activeElement;
  const previousOverflow=document.body.style.overflow;
  // Handle Escape ourselves: repeated native cancel requests may become
  // non-cancelable. Capture also covers focus moving off a disabled save field.
  const cancel=event=>{event.preventDefault();if(!controlsRef.current.busy)controlsRef.current.onClose();};
  const escape=event=>{if(event.key==="Escape"){event.preventDefault();event.stopPropagation();if(!controlsRef.current.busy)controlsRef.current.onClose();}};
  dialog.addEventListener("cancel",cancel);
  window.addEventListener("keydown",escape,true);
  dialog.showModal();
  document.body.style.overflow="hidden";
  dialog.querySelector("[data-autofocus]")?.focus();
  return ()=>{
   dialog.removeEventListener("cancel",cancel);
   window.removeEventListener("keydown",escape,true);
   dialog.close();
   document.body.style.overflow=previousOverflow;
   if(previousFocus?.isConnected)previousFocus.focus();
  };
 },[]);
 function trapTab(event){
  if(event.key!=="Tab")return;
  const controls=[...event.currentTarget.querySelectorAll("button, a[href], input, select, textarea, [tabindex]")].filter(control=>control.tabIndex>=0&&!control.matches(":disabled")&&control.getClientRects().length);
  if(!controls.length){event.preventDefault();return;}
  const first=controls[0], last=controls[controls.length-1], active=document.activeElement;
  if(event.shiftKey&&active===first){event.preventDefault();last.focus();}
  else if(!event.shiftKey&&active===last){event.preventDefault();first.focus();}
 }
 function backdrop(event){
  if(event.target!==event.currentTarget||busy)return;
  const bounds=event.currentTarget.getBoundingClientRect();
  if(event.clientX<bounds.left||event.clientX>bounds.right||event.clientY<bounds.top||event.clientY>bounds.bottom)onClose();
 }
 return createPortal(<dialog ref={dialogRef} closedby="none" className="modal-overlay" aria-modal="true" aria-labelledby={titleId} aria-busy={busy} onClick={backdrop} onKeyDown={trapTab}>
  <section className="hub-card modal-panel">
   <header className="panel-heading"><h2 id={titleId}>{title}</h2><button type="button" className="secondary-button" disabled={busy} aria-label={`Close ${title}`} onClick={onClose}>Close</button></header>
   {children}
  </section>
 </dialog>,document.body);
}
