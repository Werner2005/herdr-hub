import React, { useEffect, useState } from "react";

export function APITokensPanel({ api, action, busy }) {
 const [items,setItems]=useState([]), [name,setName]=useState(""), [created,setCreated]=useState(null), [error,setError]=useState("");
 async function refresh(){setItems(await api("api-tokens"));}
 useEffect(()=>{refresh().catch(e=>setError(e.message));},[]);
 return <section className="hub-grid">
  <form className="hub-card" onSubmit={e=>{e.preventDefault();setCreated(null);action(async()=>{const token=await api("api-tokens","POST",{name});setCreated(token);setName("");await refresh();});}}>
   <h2>Create API token</h2><p className="intro-copy">Tokens grant full API access as your admin account. Store them securely; the token is shown only once. Deleting or demoting the account revokes its tokens.</p>
   <label>Token name<input required maxLength={100} disabled={busy} value={name} onChange={e=>setName(e.target.value)} placeholder="Automation"/></label>
   <button className="primary-button" disabled={busy||!name.trim()}>Create token</button>
   {created&&<section aria-label="New API token"><h3>{created.name}</h3><p>Save this token now. You cannot view it again.</p><label>API token<input readOnly value={created.token} autoComplete="off"/></label><p>HTTP header: <code>Authorization: Bearer &lt;token&gt;</code></p><button type="button" className="secondary-button" onClick={()=>setCreated(null)}>Hide token</button></section>}
  </form>
  <section className="hub-card"><div className="panel-heading"><h2>API tokens ({items.length})</h2><button className="secondary-button" disabled={busy} onClick={()=>action(refresh)}>Refresh</button></div>
   {error&&<p role="alert">{error}</p>}
   {items.length===0&&<p>No API tokens created.</p>}
   {items.map(item=><div key={item.id}><h3>{item.name}</h3><p className="intro-copy">{item.username} · Created {new Date(item.created_at).toLocaleString()}</p><button className="secondary-button" disabled={busy} onClick={()=>{if(confirm(`Revoke API token ${item.name}? Applications using it will lose access immediately.`))action(async()=>{await api(`api-tokens/${item.id}`,"DELETE");if(created?.id===item.id)setCreated(null);await refresh();});}}>Revoke token</button></div>)}
  </section>
 </section>;
}
