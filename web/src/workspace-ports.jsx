import React, { useState } from "react";
import { validPortDraft } from "./workspace-ports.js";

export function WorkspacePorts({ item, api, action, busy }) {
 const [name,setName]=useState(""), [protocol,setProtocol]=useState("HTTP"), [port,setPort]=useState("3000");
 const disabled=busy||["deleting","undeploying"].includes(item.status);
 return <section aria-label={`Exposed ports for ${item.name}`}>
  <h4>Exposed ports</h4>
   <p className="intro-copy">Exports are accessible to anyone who can reach the gateway, without Hub authentication. Protect sensitive apps yourself. Only expose trusted HTTP apps: browser cookies can be shared across ports of the workspace hostname. Apps must listen on 0.0.0.0, not localhost. HTTP exports use HTTPS at the gateway.</p>
  <ul className="ssh-key-list">{(item.ports||[]).map(mapping=><li key={mapping.name}>
   <div><strong>{mapping.name}</strong> · {mapping.protocol} → {mapping.internal_port}<br/>
     {!mapping.address?<span>Awaiting external port allocation</span>:mapping.protocol==="HTTP"?<a href={mapping.address} target="_blank" rel="noreferrer">{mapping.address}</a>:<code>{mapping.address}</code>}
    <p role="status">{mapping.deleting?"Deleting":mapping.status}{mapping.error?` — ${mapping.error}`:""}</p>
   </div>
   <button type="button" className="secondary-button" disabled={disabled} onClick={()=>{if(confirm(`Remove ${mapping.name}? External connections to this export will stop.`))action(()=>api(`workspaces/${item.name}/ports/${mapping.name}`,"DELETE"));}}>Remove</button>
  </li>)}</ul>
  <form onSubmit={e=>{e.preventDefault();action(async()=>{await api(`workspaces/${item.name}/ports`,"POST",{name,protocol,internal_port:Number(port)});setName("");});}}>
   <label>Export name<input required maxLength={40} pattern="[a-z]([a-z0-9-]*[a-z0-9])?" value={name} disabled={disabled} onChange={e=>setName(e.target.value)} placeholder="my-app"/></label>
   <label>Protocol<select disabled={disabled} value={protocol} onChange={e=>setProtocol(e.target.value)}>{["HTTP","TCP","UDP"].map(value=><option key={value}>{value}</option>)}</select></label>
   <label>Internal port<input required type="number" min="1" max="65535" step="1" disabled={disabled} value={port} onChange={e=>setPort(e.target.value)}/></label>
   <p className="intro-copy">All exports use {item.name}'s workspace hostname with an automatically assigned external port. HTTP exports use HTTPS on that port; the workspace web UI stays on port 443. Mappings are retained while undeployed. Ready means the gateway accepted the route, not that your app is running.</p>
   <button className="secondary-button" disabled={disabled||!validPortDraft(name,protocol,port,item.ports||[])}>Expose port</button>
  </form>
 </section>;
}
