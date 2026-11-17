import React, { useState } from "react";
import { NamePicker } from "./name-picker.jsx";
import { ExpandableEntry } from "./expandable-entry.jsx";
import { attachableVolumes } from "./volume-selection.js";

const names = value => value.split(/[\s,]+/).filter(Boolean);
const canManage = (user, item) => user.role === "admin" || item.owner === user.username;

export function StoragePanel({ user, users, volumes, groups, config, api, action, busy }) {
 const [name,setName]=useState(""), [size,setSize]=useState("10Gi"), [storageClass,setStorageClass]=useState(config.storage_class||"");
 const [drafts,setDrafts]=useState({}), [sizes,setSizes]=useState({}), [sizeMessages,setSizeMessages]=useState({});
 const [owners,setOwners]=useState({});
 function draft(volume,field) { return drafts[volume.name]?.[field] ?? volume[field].join(", "); }
 function edit(volume,field,value) { setDrafts({...drafts,[volume.name]:{...drafts[volume.name],[field]:value}}); }
 return <section className="hub-grid">
  <form className="hub-card" onSubmit={e=>{e.preventDefault();action(async()=>{await api("volumes","POST",{name,storage_size:size,storage_class:storageClass});setName("");});}}>
   <h2>Create a shared PVC</h2>
   <label>Volume name<input required pattern="[a-z]([a-z0-9-]*[a-z0-9])?" maxLength={40} value={name} onChange={e=>setName(e.target.value)}/></label>
   <label>Disk size<input required pattern="[1-9][0-9]*Gi" value={size} onChange={e=>setSize(e.target.value)}/></label>
    <label>StorageClass<input disabled={user.role!=="admin"} value={storageClass} onChange={e=>setStorageClass(e.target.value)} placeholder="Cluster default"/></label>
    {config.security_limits&&<p className="intro-copy">Owner budgets: {config.security_limits.volume_gi} Gi per shared volume, {config.security_limits.user_storage_gi} Gi total storage, {config.security_limits.user_volumes} shared volumes. Admin allocations may override these budgets.</p>}
   <p className="intro-copy">The StorageClass must support ReadWriteMany (RWX), such as NFS or CephFS. All authorized workspaces have read/write access. Mounted at /mnt/shared/{name||"volume-name"}.</p>
   <p className="intro-copy">If creation fails, submit the same name and settings to retry provisioning.</p>
   <button className="primary-button" disabled={busy}>Create PVC</button>
  </form>
  <section className="hub-card"><h2>Available volumes ({volumes.length})</h2>
   {volumes.length===0&&<p>No shared volumes available.</p>}
    {volumes.map(v=><ExpandableEntry key={v.name} name={v.name} badge={v.status} summary={`${v.storage_size} · Owner: ${v.owner} · ${v.users.length} user${v.users.length===1?"":"s"} · ${v.groups.length} group${v.groups.length===1?"":"s"}`}>
     <p>StorageClass: {v.storage_class||"default"}</p>
    <p><code>/mnt/shared/{v.name}</code></p>
    <p className="intro-copy">Users: {v.users.join(", ")||"none"} · Groups: {v.groups.join(", ")||"none"}</p>
    {canManage(user,v)&&<>
     {user.role==="admin"&&<form onSubmit={e=>{e.preventDefault();action(async()=>{await api(`volumes/${v.name}/owner`,"PATCH",{owner:owners[v.name]??v.owner});setOwners(current=>{const next={...current};delete next[v.name];return next;});});}}>
      <label>Volume owner<select required disabled={busy} value={owners[v.name]??v.owner} onChange={e=>setOwners({...owners,[v.name]:e.target.value})}>{users.map(account=><option key={account.username} value={account.username}>{account.username}</option>)}</select></label>
      <p className="intro-copy">Existing shares and mounts are retained. The previous owner keeps access as an explicit user share.</p>
      <button className="secondary-button" disabled={busy||(owners[v.name]??v.owner)===v.owner}>Change owner</button>
     </form>}
     {v.status==="unavailable"&&<button className="secondary-button" disabled={busy} onClick={()=>action(()=>api("volumes","POST",{name:v.name,owner:v.owner,storage_size:v.storage_size,storage_class:v.storage_class}))}>Retry PVC creation</button>}
     <form onSubmit={e=>{e.preventDefault();setSizeMessages(current=>({...current,[v.name]:""}));action(async()=>{await api(`volumes/${v.name}/size`,"PATCH",{storage_size:sizes[v.name]??v.storage_size});setSizes(current=>{const next={...current};delete next[v.name];return next;});setSizeMessages(current=>({...current,[v.name]:"Expansion requested. The storage provider may take time to make the new capacity available."}));});}}>
      <label>Expand shared disk<input required pattern="[1-9][0-9]*Gi" disabled={busy||v.status==="unavailable"} value={sizes[v.name]??v.storage_size} onChange={e=>{setSizes({...sizes,[v.name]:e.target.value});setSizeMessages({...sizeMessages,[v.name]:""});}} placeholder="20Gi"/></label>
      <p className="intro-copy">Increase only; shared disks cannot shrink. The StorageClass must allow expansion. Existing data and workspace mounts are retained.</p>
      <button className="secondary-button" disabled={busy||v.status==="unavailable"}>Expand disk</button>
      {sizeMessages[v.name]&&<p role="status" className="intro-copy">{sizeMessages[v.name]}</p>}
     </form>
     <form onSubmit={e=>{e.preventDefault();action(async()=>{await api(`volumes/${v.name}/shares`,"PUT",{users:names(draft(v,"users")),groups:names(draft(v,"groups"))});setDrafts(current=>{const next={...current};delete next[v.name];return next;});});}}>
      <NamePicker label="Share with users" kind="users" value={draft(v,"users")} onChange={value=>edit(v,"users",value)} api={api} disabled={busy}/>
      <NamePicker label="Share with groups" kind="groups" value={draft(v,"groups")} onChange={value=>edit(v,"groups",value)} api={api} disabled={busy}/>
      <p className="intro-copy">Comma-separated account and group names. Your groups: {groups.map(g=>g.name).join(", ")||"none"}. Group owners and members receive access.</p>
      <button className="secondary-button" disabled={busy}>Save sharing</button>
     </form>
     <p className="intro-copy">Detach from all workspaces and wait for old pods to terminate before removing shares or deleting the PVC.</p>
     <button className="secondary-button" disabled={busy} onClick={()=>{if(confirm(`Permanently delete PVC ${v.name} and its data?`))action(()=>api(`volumes/${v.name}`,"DELETE"));}}>Delete PVC</button>
    </>}
    </ExpandableEntry>)}
  </section>
 </section>;
}

export function GroupsPanel({ user, groups, api, action, busy }) {
 const [name,setName]=useState(""), [drafts,setDrafts]=useState({});
 return <section className="hub-grid">
  <form className="hub-card" onSubmit={e=>{e.preventDefault();action(async()=>{await api("groups","POST",{name});setName("");});}}>
   <h2>Create a group</h2>
   <label>Group name<input required pattern="[A-Za-z]([A-Za-z0-9-]*[A-Za-z0-9])?" maxLength={40} value={name} onChange={e=>setName(e.target.value)}/></label>
   <p className="intro-copy">You own the group and manage its membership. Share a PVC with the group to give all members access.</p>
   <button className="primary-button" disabled={busy}>Create group</button>
  </form>
  <section className="hub-card"><h2>Groups ({groups.length})</h2>
   {groups.length===0&&<p>No groups yet.</p>}
    {groups.map(g=><ExpandableEntry key={g.name} name={g.name} summary={`Owner: ${g.owner} · ${g.members.length} member${g.members.length===1?"":"s"}`}>
     <p>Members: {g.members.join(", ")||"none"}</p>
    {canManage(user,g)&&<>
     <form onSubmit={e=>{e.preventDefault();action(async()=>{await api(`groups/${g.name}/members`,"PUT",{members:names(drafts[g.name]??g.members.join(", "))});setDrafts(current=>{const next={...current};delete next[g.name];return next;});});}}>
      <NamePicker label="Members" kind="users" value={drafts[g.name]??g.members.join(", ")} onChange={value=>setDrafts({...drafts,[g.name]:value})} api={api} disabled={busy}/>
      <p className="intro-copy">Comma-separated existing usernames. Detach group-shared volumes before removing members or deleting the group.</p>
      <button className="secondary-button" disabled={busy}>Save members</button>
     </form>
     <button className="secondary-button" disabled={busy} onClick={()=>{if(confirm(`Delete group ${g.name}?`))action(()=>api(`groups/${g.name}`,"DELETE"));}}>Delete group</button>
    </>}
    </ExpandableEntry>)}
  </section>
 </section>;
}

export function WorkspaceVolumes({ item, volumes, api, action, busy }) {
 const [selected,setSelected]=useState("");
 const mounted=item.volumes||[];
 const choices=attachableVolumes(volumes,item);
 const selection=choices.some(volume=>volume.name===selected)?selected:"";
 return <section>
  <h4>Shared volumes</h4>
  <ul>{mounted.map(name=><li key={name}><code>/mnt/shared/{name}</code> <button className="secondary-button" disabled={busy||item.status==="deleting"} onClick={()=>action(()=>api(`workspaces/${item.name}/volumes`,"PUT",{volumes:mounted.filter(n=>n!==name)}))}>Detach</button></li>)}</ul>
  <form onSubmit={e=>{e.preventDefault();if(!selection)return;action(async()=>{await api(`workspaces/${item.name}/volumes`,"PUT",{volumes:[...mounted,selection]});setSelected("");});}}>
   <label>Attach a PVC<select required value={selection} onChange={e=>setSelected(e.target.value)} disabled={busy||item.status==="deleting"}><option value="">Select a volume</option>{choices.map(v=><option key={v.name} value={v.name}>{v.name} ({v.storage_size})</option>)}</select></label>
   <p className="intro-copy">Attaching or detaching restarts this workspace and disconnects active sessions. The workspace owner must have volume access. Data is retained when detached or when the runtime is deleted.</p>
   <button className="secondary-button" disabled={busy||!selection||item.status==="deleting"}>Attach volume</button>
  </form>
 </section>;
}
