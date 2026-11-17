import React, { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import "./styles.css";
import { StoragePanel, GroupsPanel, WorkspaceVolumes } from "./shared-storage.jsx";
import { ExpandableEntry } from "./expandable-entry.jsx";
import { ResourceFields, WorkspaceResourceSettings } from "./workspace-resources.jsx";
import { WorkspacePorts } from "./workspace-ports.jsx";
import { ConfigurationsPanel, WorkspaceConfigurations, CreateWorkspaceConfigurations } from "./configurations.jsx";
import { APITokensPanel } from "./api-tokens.jsx";
import { EgressEditor, WorkspaceEgress } from "./egress.jsx";

function App() {
 const [user,setUser]=useState(null), [items,setItems]=useState([]), [users,setUsers]=useState([]);
 const [config,setConfig]=useState({}), [busy,setBusy]=useState(false), [error,setError]=useState("");
  const [resourceDefaults,setResourceDefaults]=useState({}), [settingsMessage,setSettingsMessage]=useState("");
  const [securityLimits,setSecurityLimits]=useState({});
 const [egressDefaults,setEgressDefaults]=useState(null);
 const [newResources,setNewResources]=useState(null);
 const [username,setUsername]=useState(""), [password,setPassword]=useState("");
 const [name,setName]=useState(""), [key,setKey]=useState(""), [owner,setOwner]=useState(""), [size,setSize]=useState("10Gi");
 const [newUsername,setNewUsername]=useState(""), [newPassword,setNewPassword]=useState(""), [role,setRole]=useState("user");
 const [webToken,setWebToken]=useState(null), [sizes,setSizes]=useState({});
  const [volumes,setVolumes]=useState([]), [groups,setGroups]=useState([]);
 const [configurations,setConfigurations]=useState([]);
 const [newConfigurations,setNewConfigurations]=useState([]);
 const [sshKeys,setSSHKeys]=useState({}), [keyMessages,setKeyMessages]=useState({});
 const [view,setView]=useState("workspaces"), [userPasswords,setUserPasswords]=useState({}), [userMessages,setUserMessages]=useState({});
 const [dark,setDark]=useState(()=>localStorage.getItem("herdr-theme") ? localStorage.getItem("herdr-theme")==="dark" : window.matchMedia("(prefers-color-scheme: dark)").matches);
 const admin=user?.role==="admin";
 useEffect(()=>{document.documentElement.dataset.theme=dark?"dark":"light";localStorage.setItem("herdr-theme",dark?"dark":"light");},[dark]);
 async function api(path,method="GET",body) {
  const r=await fetch(`/api/${path}`,{method,credentials:"same-origin",headers:{"Content-Type":"application/json"},body:body===undefined?undefined:JSON.stringify(body)});
  if(r.status===204)return;
  const data=await r.json();if(!r.ok){if(r.status===401)clearSession();throw new Error(data.error||`Request failed (${r.status})`);}return data;
 }
 async function refresh(){const [workspaces,storage,teams,configurationItems,egress]=await Promise.all([api("workspaces"),api("volumes"),api("groups"),api("configurations"),api("egress-defaults")]);setItems(workspaces);setVolumes(storage);setGroups(teams);setConfigurations(configurationItems);setEgressDefaults(egress);}
  async function connect(u){const c=await api("config");setConfig(c);setResourceDefaults(c.resource_defaults);setSecurityLimits(c.security_limits);setSettingsMessage("");setSize(c.storage_size);await refresh();if(u.role==="admin")setUsers(await api("users"));setOwner(u.username);setUser(u);}
 useEffect(()=>{api("me").then(connect).catch(()=>{});},[]);
 useEffect(()=>{if(!user)return;const timer=setInterval(()=>refresh().catch(e=>setError(e.message)),10000);return()=>clearInterval(timer);},[user]);
 async function action(fn){setBusy(true);setError("");try{await fn();await refresh();}catch(e){setError(e.message);await refresh().catch(()=>{});}finally{setBusy(false);}}
 async function login(e){e.preventDefault();setBusy(true);setError("");try{const u=await api("login","POST",{username,password});setPassword("");await connect(u);}catch(e){setError(e.message);}finally{setBusy(false);}}
 function clearSession(){setUser(null);setItems([]);setUsers([]);setVolumes([]);setGroups([]);setConfigurations([]);setNewConfigurations([]);setEgressDefaults(null);setWebToken(null);setSSHKeys({});setKeyMessages({});setUserPasswords({});setUserMessages({});setNewPassword("");setSizes({});setNewResources(null);setView("workspaces");}
 function navigate(next){setView(next);setError("");setWebToken(null);}
 async function logout(){setBusy(true);setError("");try{await api("logout","POST");clearSession();}catch(e){setError(e.message);}finally{setBusy(false);}}
 function create(e){e.preventDefault();action(async()=>{await api("workspaces","POST",{name,ssh_public_key:key,owner,storage_size:size,configurations:newConfigurations,...(newResources?{resources:newResources}:{})});setName("");setKey("");setNewResources(null);setNewConfigurations([]);setView("workspaces");});}
 function createUser(e){e.preventDefault();action(async()=>{await api("users","POST",{username:newUsername,password:newPassword,role});setNewUsername("");setNewPassword("");setUsers(await api("users"));});}
 function manageUser(username,body){action(async()=>{await api(`users/${username}`,"PATCH",body);setUsers(await api("users"));if(body.password)setUserPasswords(passwords=>({...passwords,[username]:""}));setUserMessages(messages=>({...messages,[username]:body.password?"Password updated. Other sessions have been signed out.":"Role updated."}));});}
 function savedKeys(item){return (item.ssh_public_key||"").split("\n").map(key=>key.trim()).filter(Boolean);}
 function usageSummary(item){if(item.status==="undeployed")return "CPU / memory: not running";if(!item.usage)return "CPU / memory usage: unavailable";return `CPU: ${Math.round(item.usage.cpu_millicores*10)/10}m · Memory: ${(item.usage.memory_bytes/1048576).toFixed(1)} MiB`;}
 function saveKeys(item,keys,added=false){setKeyMessages(messages=>({...messages,[item.name]:""}));action(async()=>{await api(`workspaces/${item.name}/ssh-key`,"PATCH",{ssh_public_key:keys});if(added)setSSHKeys(drafts=>({...drafts,[item.name]:""}));setKeyMessages(messages=>({...messages,[item.name]:"SSH keys saved. The workspace is restarting to apply them."}));});}
 return <div className="app-shell">
  <header className="topbar"><a className="brand" href="/">Herdr <span className="brand-hub">Hub</span></a><span className="topbar-context">{user?`${user.username} · ${user.role}`:"Kubernetes workspaces"}</span></header>
  <aside className="sidebar" aria-label="Sidebar">
   <nav aria-label="Main navigation">
    {user?<><button className="nav-button" aria-current={view==="workspaces"?"page":undefined} onClick={()=>navigate("workspaces")}>Workspaces</button>{admin&&<><button className="nav-button" aria-current={view==="create"?"page":undefined} onClick={()=>navigate("create")}>Create workspace</button><button className="nav-button" disabled={busy} aria-current={view==="users"?"page":undefined} onClick={()=>{navigate("users");action(async()=>setUsers(await api("users")));}}>Users</button></>}</>:<span className="nav-label">Sign in</span>}
    </nav>
      {admin&&<nav aria-label="Administration"><button className="nav-button" disabled={busy} aria-current={view==="api-tokens"?"page":undefined} onClick={()=>navigate("api-tokens")}>API tokens</button><button className="nav-button" disabled={busy} aria-current={view==="settings"?"page":undefined} onClick={()=>{navigate("settings");setSettingsMessage("");action(async()=>{const c=await api("config");setConfig(c);setResourceDefaults(c.resource_defaults);setSecurityLimits(c.security_limits);});}}>Workspace defaults</button></nav>}
    {user&&<nav aria-label="Sharing"><button className="nav-button" aria-current={view==="storage"?"page":undefined} onClick={()=>navigate("storage")}>Shared storage</button><button className="nav-button" aria-current={view==="groups"?"page":undefined} onClick={()=>navigate("groups")}>Groups</button></nav>}
     {user&&<nav aria-label="Configuration"><button className="nav-button" aria-current={view==="configurations"?"page":undefined} onClick={()=>navigate("configurations")}>ConfigMaps &amp; Secrets</button><button className="nav-button" aria-current={view==="egress"?"page":undefined} onClick={()=>navigate("egress")}>Network access</button></nav>}
   <div className="sidebar-actions"><button className="secondary-button" aria-pressed={dark} onClick={()=>setDark(!dark)}>{dark?"Light mode":"Dark mode"}</button>{user&&<button disabled={busy} className="secondary-button" onClick={logout}>Sign out</button>}</div>
  </aside>
  <main className="page" id="main-content">
       <section className="page-heading"><h1>{!user?"Welcome to Herdr Hub":view==="egress"?"Network access":view==="api-tokens"?"API tokens":view==="configurations"?"ConfigMaps & Secrets":view==="settings"?"Workspace defaults":view==="storage"?"Shared storage":view==="groups"?"Groups":view==="users"?"User management":view==="create"?"Create a workspace":admin?"All workspaces":"My workspaces"}</h1><p className="intro-copy">{view==="users"&&admin?"Manage accounts, roles, and passwords.":"Persistent workspaces with web UI and dedicated SSH access."}</p></section>
   {error&&<div className="error-banner" role="alert">{error}</div>}
    {!user?<form className="hub-card login-card" onSubmit={login}><h2>Sign in</h2><label>Username<input required value={username} onChange={e=>setUsername(e.target.value)} autoComplete="username"/></label><label>Password<input type="password" required value={password} onChange={e=>setPassword(e.target.value)} autoComplete="current-password"/></label><button className="primary-button" disabled={busy}>Sign in</button></form>:<>
        {view==="storage"&&<StoragePanel user={user} users={users} volumes={volumes} groups={groups} config={config} api={api} action={action} busy={busy}/>}
        {view==="configurations"&&<ConfigurationsPanel user={user} configurations={configurations} api={api} action={action} busy={busy}/>}
        {view==="egress"&&<><section className="hub-card"><h2>Default outbound rules — all workspaces</h2><p className="intro-copy">{admin?"Admin rules apply immediately to all existing and future workspaces.":"Managed by your administrator. You cannot change these defaults."}</p><EgressEditor settings={egressDefaults} endpoint="egress-defaults" api={api} action={action} busy={busy} editable={admin}/></section><section className="hub-card"><h2>{admin?"Workspace-specific rules":"My workspace-specific rules"}</h2>{items.map(item=><ExpandableEntry key={item.name} name={item.name} summary={`${item.egress?.rules?.length||0} additional outbound rules`}><WorkspaceEgress item={item} api={api} action={action} busy={busy}/></ExpandableEntry>)}</section></>}
        {view==="groups"&&<GroupsPanel user={user} groups={groups} api={api} action={action} busy={busy}/>}
        {admin&&view==="api-tokens"&&<APITokensPanel api={api} action={action} busy={busy}/>}
      {admin&&view==="settings"&&<form className="hub-card" onSubmit={e=>{e.preventDefault();setSettingsMessage("");action(async()=>{const c=await api("config","PATCH",{resource_defaults:resourceDefaults,security_limits:securityLimits});setConfig(c);setResourceDefaults(c.resource_defaults);setSecurityLimits(c.security_limits);setSettingsMessage("Defaults and security limits saved. Existing allocations are not automatically reduced.");});}}><h2>CPU and memory</h2>
       <p className="intro-copy">Requests reserve capacity for scheduling. Limits cap usage. These defaults apply to new workspaces only; existing workspaces keep their saved values, including after undeploying and deploying again.</p>
       {[["cpu_request","CPU request","500m"],["cpu_limit","CPU limit (optional)","2"],["memory_request","Memory request","1Gi"],["memory_limit","Memory limit","4Gi"]].map(([field,label,placeholder])=><label key={field}>{label}<input required={field!=="cpu_limit"} disabled={busy} maxLength={32} value={resourceDefaults[field]??""} onChange={e=>{setResourceDefaults({...resourceDefaults,[field]:e.target.value});setSettingsMessage("");}} placeholder={placeholder}/></label>)}
        <p className="intro-copy">CPU: cores (2, 0.5) or millicores (500m). Memory: whole bytes or Ki/Mi/Gi/Ti (512Mi, 4Gi). Requests cannot exceed limits. Defaults must have a finite CPU limit within the security maximums.</p>
        <h3>Owner security maximums</h3>
        <p className="intro-copy">Enforced on non-admin resource edits and storage allocations. Total storage includes owned workspace homes and shared volumes recorded in Hub. Admins may override these budgets.</p>
        {[["cpu","Maximum CPU per workspace",false],["memory","Maximum memory per workspace",false],["volume_gi","Maximum shared-volume size (Gi)",true],["user_storage_gi","Maximum total storage per owner (Gi)",true],["user_volumes","Maximum shared volumes per owner",true]].map(([field,label,numeric])=><label key={field}>{label}<input required type={numeric?"number":"text"} min={numeric?1:undefined} step={numeric?1:undefined} disabled={busy} value={securityLimits[field]??""} onChange={e=>setSecurityLimits({...securityLimits,[field]:numeric?Number(e.target.value):e.target.value})}/></label>)}
        <button className="primary-button" disabled={busy}>Save defaults</button>{settingsMessage&&<p className="intro-copy" role="status">{settingsMessage}</p>}
      </form>}
     {admin&&view==="create"&&<form className="hub-card" onSubmit={create}><h2>Workspace details</h2>
      <label>Workspace name<input required pattern="[a-z]([a-z0-9-]*[a-z0-9])?" maxLength={40} value={name} onChange={e=>setName(e.target.value)} placeholder="my-project"/></label>
      <p className="intro-copy">https://{name||"my-project"}.{config.domain}</p>
       <label>Owner<select required disabled={busy} value={owner} onChange={e=>{setOwner(e.target.value);setNewConfigurations([]);}}>{users.map(u=><option key={u.username} value={u.username}>{u.username} ({u.role})</option>)}</select></label>
       <label>SSH public keys<textarea required rows={4} value={key} onChange={e=>setKey(e.target.value)} placeholder="ssh-ed25519 AAAA… user@computer"/></label>
       <p className="intro-copy">Paste one or more public keys. Extra spaces, tabs, and wrapped key data are normalized automatically.</p>
      <label>Home disk size<input required pattern="[1-9][0-9]*Gi" value={size} onChange={e=>setSize(e.target.value)} placeholder="20Gi"/></label>
       <p className="intro-copy">A dedicated SSH port is allocated automatically.</p>
        <h4>Workspace CPU and memory</h4>
        <ResourceFields value={newResources??config.resource_defaults} onChange={setNewResources} disabled={busy}/>
         <p className="intro-copy">Starts with the admin defaults. Edit these values to override them for this workspace only.</p>
       <CreateWorkspaceConfigurations owner={owner} configurations={configurations} value={newConfigurations} onChange={setNewConfigurations} busy={busy}/>
       <button className="primary-button" disabled={busy||newConfigurations.some(mapping=>(mapping.files||[]).length+(mapping.env||[]).length===0||!configurations.some(item=>item.name===mapping.configuration&&item.owner===owner&&!item.global&&item.ready))||configurations.some(item=>item.global&&!item.ready)}>Create workspace</button>
     </form>}
     {admin&&view==="users"&&<section className="hub-grid"><form className="hub-card" onSubmit={createUser}><h2>Create an account</h2>
      <label>Username<input required pattern="[A-Za-z]([A-Za-z0-9-]*[A-Za-z0-9])?" maxLength={40} value={newUsername} onChange={e=>setNewUsername(e.target.value)} autoComplete="off"/></label>
      <label>Password<input type="password" required minLength={12} maxLength={72} value={newPassword} onChange={e=>setNewPassword(e.target.value)} autoComplete="new-password"/></label>
      <label>Role<select value={role} onChange={e=>setRole(e.target.value)}><option value="user">User</option><option value="admin">Admin</option></select></label>
      <button className="primary-button" disabled={busy}>Create account</button>
     </form><section className="hub-card"><div className="panel-heading"><h2>Accounts ({users.length})</h2><button className="secondary-button" disabled={busy} onClick={()=>action(async()=>setUsers(await api("users")))}>Refresh</button></div>
       {users.map(account=>{const self=account.username===user.username;const assigned=items.filter(item=>item.owner===account.username).length;return <ExpandableEntry key={account.username} name={`${account.username}${self?" (you)":""}`} badge={account.role} summary={`${assigned} assigned workspace${assigned===1?"":"s"}`}>
       <label>Role<select disabled={busy||self} value={account.role} onChange={e=>manageUser(account.username,{role:e.target.value})}><option value="user">User</option><option value="admin">Admin</option></select></label>
       <form onSubmit={e=>{e.preventDefault();manageUser(account.username,{password:userPasswords[account.username]});}}><label>New password<input type="password" required minLength={12} maxLength={72} autoComplete="new-password" disabled={busy} value={userPasswords[account.username]??""} onChange={e=>{setUserPasswords({...userPasswords,[account.username]:e.target.value});setUserMessages({...userMessages,[account.username]:""});}}/></label><div className="hub-actions"><button className="secondary-button" disabled={busy}>Reset password</button><button type="button" className="secondary-button" disabled={busy||self||assigned>0} onClick={()=>{if(confirm(`Delete user ${account.username}?`))action(async()=>{await api(`users/${account.username}`,"DELETE");setUsers(await api("users"));if(owner===account.username)setOwner(user.username);});}}>Delete user</button></div></form>
       {assigned>0&&<p className="intro-copy">Reassign workspaces before deleting this account.</p>}
       {userMessages[account.username]&&<p role="status" className="intro-copy">{userMessages[account.username]}</p>}
       </ExpandableEntry>;})}
     </section></section>}
     {view==="workspaces"&&<section className="hub-card"><div className="panel-heading"><h2>Workspaces ({items.length})</h2><button className="secondary-button" disabled={busy} onClick={()=>action(async()=>{})}>Refresh</button></div>
      {items.length===0&&<p>{admin?"Create a workspace to get started.":"No workspaces assigned. Ask your administrator to create one."}</p>}
        {items.map(item=><ExpandableEntry key={item.name} name={item.name} badge={item.status} summary={`${usageSummary(item)} · Home disk: ${item.storage_size}${admin?` · Owner: ${item.owner||"unassigned"}`:""} · ${(item.volumes||[]).length} shared volume${(item.volumes||[]).length===1?"":"s"}${item.error?" · Error — expand for details":""}`}>
         <p className="intro-copy">Current usage — {usageSummary(item)}{item.usage?` · Sampled ${new Date(item.usage.sampled_at).toLocaleTimeString()}`:""}. Refreshed every 10 seconds. CPU is sampled usage, not reserved capacity; 1000m equals one core. Memory is the container working set.</p>
         <WorkspaceVolumes item={item} volumes={volumes} api={api} action={action} busy={busy||["undeployed","undeploying"].includes(item.status)}/>
          <WorkspaceConfigurations item={item} configurations={configurations} api={api} action={action} busy={busy||["deleting","undeploying"].includes(item.status)}/>
           <WorkspacePorts item={item} api={api} action={action} busy={busy}/>
           <WorkspaceEgress item={item} api={api} action={action} busy={busy}/>
        {admin&&<label>Assign owner<select value={item.owner} disabled={busy} onChange={e=>{const owner=e.target.value;if(confirm("Transfer ownership? The workspace will stop, its web token will rotate, and all SSH keys will be removed. Retained home files are not sanitized and must be trusted."))action(()=>api(`workspaces/${item.name}/owner`,"PATCH",{owner}));}}><option value="" disabled>Unassigned</option>{users.map(u=><option key={u.username} value={u.username}>{u.username}</option>)}</select></label>}
        {!["undeployed","undeploying"].includes(item.status)&&<><a href={item.url} target="_blank" rel="noreferrer">Open Herdr web UI ↗</a>
         {item.ssh_port>0&&<p><code>ssh -p {item.ssh_port} dev@{item.ssh_host||"gateway-address"}</code></p>}</>}
        {item.status==="undeployed"&&<p className="intro-copy">Undeployed to save runtime resources. Your home files, credentials, and shared-volume assignments are retained. Deploy to reconnect.</p>}
         {item.resources&&<p className="intro-copy">CPU request: {item.resources.cpu_request} · limit: {item.resources.cpu_limit||"none"}. Memory request: {item.resources.memory_request} · limit: {item.resources.memory_limit}.</p>}
          <WorkspaceResourceSettings item={item} defaults={config.resource_defaults} api={api} action={action} busy={busy} limits={admin?undefined:config.security_limits}/>
         <h4>SSH public keys ({savedKeys(item).length})</h4>
         <ul className="ssh-key-list">{savedKeys(item).map((saved,index)=><li key={`${index}-${saved}`}><code>{saved}</code><button type="button" className="secondary-button" aria-label={`Remove SSH key ${index+1} from ${item.name}`} disabled={busy||item.status==="deleting"} onClick={()=>saveKeys(item,savedKeys(item).filter((_,i)=>i!==index).join("\n"))}>Remove</button></li>)}</ul>
         {savedKeys(item).length===0&&<p className="intro-copy">No SSH keys. Add a key to enable SSH access.</p>}
         <form onSubmit={e=>{e.preventDefault();saveKeys(item,[...savedKeys(item),sshKeys[item.name]].join("\n"),true);}}>
          <label>Add SSH public keys<textarea required rows={3} disabled={busy||item.status==="deleting"} value={sshKeys[item.name]??""} onChange={e=>{setSSHKeys({...sshKeys,[item.name]:e.target.value});setKeyMessages({...keyMessages,[item.name]:""});}} placeholder="ssh-ed25519 AAAA… user@computer"/></label>
          <p className="intro-copy">Paste one or more keys; extra whitespace is normalized. Adding or removing keys restarts the workspace and disconnects active sessions. Your home files are retained. Removing every key disables SSH access.</p>
          <button className="secondary-button" disabled={busy||item.status==="deleting"||!(sshKeys[item.name]??"").trim()}>Add SSH keys</button>
         {keyMessages[item.name]&&<p role="status" className="intro-copy">{keyMessages[item.name]}</p>}
        </form>
       {item.error&&<p role="alert" className="error-banner">{item.error}</p>}
       {admin&&<form onSubmit={e=>{e.preventDefault();action(()=>api(`workspaces/${item.name}/size`,"PATCH",{storage_size:sizes[item.name]||item.storage_size}));}}><label>Expand home disk<input required pattern="[1-9][0-9]*Gi" value={sizes[item.name]??item.storage_size} onChange={e=>setSizes({...sizes,[item.name]:e.target.value})}/></label><button className="secondary-button" disabled={busy||item.status==="deleting"}>Expand disk</button></form>}
        <div className="hub-actions">
         <button className="secondary-button" disabled={busy||!["undeployed","unavailable","error","pending"].includes(item.status)} onClick={()=>action(()=>api(`workspaces/${item.name}/deploy`,"POST"))}>Deploy</button>
         <button className="secondary-button" disabled={busy||item.status==="deleting"||item.status==="undeployed"} onClick={()=>{if(confirm(`Undeploy ${item.name}? Active sessions will disconnect. Your files and settings will be retained.`))action(async()=>{await api(`workspaces/${item.name}/undeploy`,"POST");setWebToken(null);});}}>{item.status==="undeploying"?"Retry undeploy":"Undeploy"}</button>
          <button className="secondary-button" disabled={busy} onClick={()=>action(async()=>{const data=await api(`workspaces/${item.name}/token`);setWebToken({name:item.name,value:atob(data.token_base64)});})}>Web login token</button>
          {admin&&<button className="secondary-button" disabled={busy||item.status==="deleting"} onClick={()=>{if(confirm(`Rotate the web token for ${item.name}? This stops the workspace and disconnects all sessions. Deploy it again after rotation.`))action(async()=>{setWebToken(null);await api(`workspaces/${item.name}/token`,"POST");});}}>Rotate web token</button>}
         {admin&&<button className="secondary-button" disabled={busy||["deleting","undeployed","undeploying"].includes(item.status)} onClick={()=>action(()=>api(`workspaces/${item.name}/retry`,"POST"))}>Retry provisioning</button>}
        <button className="secondary-button" disabled={busy} onClick={()=>{if(confirm(`Delete runtime ${item.name}? Its home volume will be retained.`))action(async()=>{await api(`workspaces/${item.name}`,"DELETE");setWebToken(null);});}}>Delete runtime</button>
       </div>
       </ExpandableEntry>)}
     </section>}
    {view==="workspaces"&&webToken&&<section className="hub-card" aria-label="Workspace web login token"><h2>{webToken.name}: web login token</h2><p>Enter this token in the Herdr web UI sign-in screen.</p><input readOnly value={webToken.value} aria-label="Web login token"/><button className="secondary-button" onClick={()=>setWebToken(null)}>Hide</button></section>}
   </>}
  </main>
 </div>;
}
createRoot(document.getElementById("root")).render(<App/>);
