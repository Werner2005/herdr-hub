import React, { useState } from "react";

export function ResourceFields({ value, onChange, disabled, limits }) {
 return <>{[["cpu_request","CPU request","500m"],["cpu_limit",limits?"CPU limit":"CPU limit (optional)","2"],["memory_request","Memory request","1Gi"],["memory_limit","Memory limit","4Gi"]].map(([field,label,placeholder])=><label key={field}>{label}<input required={field!=="cpu_limit"||Boolean(limits)} disabled={disabled} maxLength={32} value={value?.[field]??""} onChange={e=>onChange({...value,[field]:e.target.value})} placeholder={placeholder}/></label>)}<p className="intro-copy">CPU: cores or millicores (500m). Memory: bytes or Ki/Mi/Gi/Ti (1Gi). Requests cannot exceed limits. {limits?`Finite limits are required: maximum CPU ${limits.cpu}, memory ${limits.memory}.`:"An empty CPU limit means no CPU cap."}</p></>;
}

export function WorkspaceResourceSettings({ item, defaults, api, action, busy, limits }) {
 const [draft,setDraft]=useState(null), [message,setMessage]=useState("");
 const disabled=busy||["deleting","undeploying"].includes(item.status);
 return <form onSubmit={e=>{e.preventDefault();setMessage("");action(async()=>{await api(`workspaces/${item.name}/resources`,"PATCH",draft??item.resources);setDraft(null);setMessage(item.status==="undeployed"?"Resources saved for the next deploy.":"Resources saved. Changed values restart the workspace.");});}}>
  <h4>Workspace CPU and memory</h4>
   <ResourceFields value={draft??item.resources} onChange={values=>{setDraft(values);setMessage("");}} disabled={disabled} limits={limits}/>
  <p className="intro-copy">Changing a running workspace restarts it and disconnects active sessions. Home files, credentials, and shared volumes are retained. Undeployed workspaces stay stopped.</p>
  <div className="hub-actions"><button className="secondary-button" disabled={disabled}>Save workspace resources</button><button type="button" className="secondary-button" disabled={disabled||!defaults} onClick={()=>action(async()=>{const config=await api("config");setDraft({...config.resource_defaults});setMessage("Default values copied. Save to apply them to this workspace.");})}>Use current defaults</button></div>
  {message&&<p className="intro-copy" role="status">{message}</p>}
 </form>;
}
