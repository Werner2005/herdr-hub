import React, { useState } from "react";
import { ExpandableEntry } from "./expandable-entry.jsx";
import { privateConfigurations, initialConfigurationMapping } from "./configuration-selection.js";

const emptyMapping = configuration => ({ configuration, files: [], env: [] });

function MappingFields({ value, onChange, keys, disabled }) {
 const edit = (field, index, next) => onChange({ ...value, [field]: value[field].map((row, i) => i === index ? next : row) });
 return <>
  {["files", "env"].map(field => <section key={field}>
   <h4>{field === "files" ? "Files" : "Environment variables"}</h4>
   {(value[field] || []).map((row, index) => <div key={index}>
    <label>Configuration key<select required disabled={disabled} value={row.key} onChange={e => edit(field, index, { ...row, key: e.target.value })}><option value="">Select a key</option>{keys.map(key => <option key={key}>{key}</option>)}</select></label>
    <label>{field === "files" ? "Absolute file path" : "Environment variable name"}<input required disabled={disabled} value={field === "files" ? row.path : row.name} onChange={e => edit(field, index, { ...row, [field === "files" ? "path" : "name"]: e.target.value })} placeholder={field === "files" ? "/home/dev/.config/my-tool/config.json" : "MY_API_TOKEN"}/></label>
    <button type="button" className="secondary-button" disabled={disabled} onClick={() => onChange({ ...value, [field]: value[field].filter((_, i) => i !== index) })}>Remove mapping</button>
   </div>)}
   <button type="button" className="secondary-button" disabled={disabled} onClick={() => onChange({ ...value, [field]: [...(value[field] || []), { key: keys[0] || "", [field === "files" ? "path" : "name"]: "" }] })}>Add {field === "files" ? "file" : "environment variable"}</button>
  </section>)}
  <p className="intro-copy">Read-only files may be mapped under /home/dev, /mnt/config or /mnt/secrets. SSH, Herdr runtime files and system paths are protected. Hub and startup environment variables are reserved. Changes restart affected workspaces and disconnect sessions.</p>
 </>;
}

function DataFields({ rows, onChange, secret, disabled }) {
 const [visible, setVisible] = useState(false);
 return <section>
  <h4>{secret ? "Secret values" : "Configuration contents"}</h4>
  {secret && <label><input type="checkbox" checked={visible} onChange={e => setVisible(e.target.checked)}/> Show entered secret values</label>}
  {rows.map((row, index) => <div key={index}>
   <label>Key / file name<input required disabled={disabled} maxLength={253} pattern="[A-Za-z0-9._-]+" value={row.key} onChange={e => onChange(rows.map((r, i) => i === index ? { ...r, key: e.target.value } : r))}/></label>
   <label>Value{secret && !visible ? <input type="password" autoComplete="new-password" disabled={disabled} value={row.value} onChange={e => onChange(rows.map((r, i) => i === index ? { ...r, value: e.target.value } : r))}/> : <textarea rows={4} disabled={disabled} value={row.value} onChange={e => onChange(rows.map((r, i) => i === index ? { ...r, value: e.target.value } : r))}/>}</label>
   <button type="button" className="secondary-button" disabled={disabled || rows.length === 1} onClick={() => onChange(rows.filter((_, i) => i !== index))}>Remove key</button>
  </div>)}
  <button type="button" className="secondary-button" disabled={disabled || rows.length >= 64} onClick={() => onChange([...rows, { key: "", value: "" }])}>Add key</button>
  {secret && <p className="intro-copy">Values are stored only in Kubernetes Secrets, never returned by Hub, and cleared from this form after saving. Kubernetes Secrets are not inherently encrypted at rest. Global secrets are accessible inside every workspace.</p>}
 </section>;
}

function ConfigurationEditor({ item, user, api, action, busy }) {
 const [name, setName] = useState(item?.name || ""), [kind, setKind] = useState(item?.kind || "configmap");
 const [global, setGlobal] = useState(item?.global || false), [mapping, setMapping] = useState(item?.mapping || emptyMapping(""));
 const [replace, setReplace] = useState(!item), [rows, setRows] = useState(item ? item.keys.map(key => ({ key, value: "" })) : [{ key: "config.json", value: "" }]);
 function submit(e) {
  e.preventDefault();
  action(async () => {
   const data = {};
   if (replace) for (const row of rows) { if (Object.hasOwn(data, row.key)) throw new Error("Configuration keys must be unique."); data[row.key] = row.value; }
   await api(item ? `configurations/${item.name}` : "configurations", item ? "PUT" : "POST", { name, kind, global, mapping: { ...mapping, configuration: name }, ...(replace ? { data } : {}) });
   setRows(item ? Object.keys(replace ? data : Object.fromEntries(item.keys.map(key => [key, ""]))).map(key => ({ key, value: "" })) : [{ key: "config.json", value: "" }]);
   if (!item) { setName(""); setMapping(emptyMapping("")); setGlobal(false); } else setReplace(false);
  });
 }
 const keys = replace ? rows.map(row => row.key).filter(Boolean) : item.keys;
 return <form onSubmit={submit}>
  {!item && <><label>Name<input required pattern="[a-z]([a-z0-9-]*[a-z0-9])?" maxLength={40} disabled={busy} value={name} onChange={e => setName(e.target.value)}/></label><label>Type<select disabled={busy} value={kind} onChange={e => { setKind(e.target.value); setRows([{ key: "config.json", value: "" }]); }}><option value="configmap">ConfigMap</option><option value="secret">Secret</option></select></label></>}
  {item && <label><input type="checkbox" disabled={busy} checked={replace} onChange={e => setReplace(e.target.checked)}/> Replace all stored values</label>}
  {replace && <DataFields rows={rows} onChange={setRows} secret={kind === "secret"} disabled={busy}/>}
  {user.role === "admin" && <label><input type="checkbox" disabled={busy} checked={global} onChange={e => setGlobal(e.target.checked)}/> Automatically map to ALL workspaces</label>}
  <h3>{global ? "Global mappings" : "Suggested mappings (optional)"}</h3>
  <MappingFields value={mapping} onChange={setMapping} keys={keys} disabled={busy}/>
  {item && !item.ready && <p role="alert">Save again with all values to retry failed provisioning.</p>}
  <button className="primary-button" disabled={busy}>{item ? "Save configuration" : "Create configuration"}</button>
 </form>;
}

function MappingSummary({ mapping }) {
 return <ul>{(mapping.files || []).map(f => <li key={f.path}><code>{f.key}</code> → <code>{f.path}</code></li>)}{(mapping.env || []).map(e => <li key={e.name}><code>{e.key}</code> → env <code>{e.name}</code></li>)}</ul>;
}

export function ConfigurationsPanel({ user, configurations, api, action, busy }) {
 return <section className="hub-grid">
  <section className="hub-card"><h2>Create ConfigMap or Secret</h2><p className="intro-copy">Private configurations can be mapped only to workspaces owned by their creator. Names are unique across Hub. Admins can apply global mappings to existing and future workspaces.</p><ConfigurationEditor user={user} api={api} action={action} busy={busy}/></section>
  <section className="hub-card"><h2>Configurations ({configurations.length})</h2>
   {configurations.map(item => <ExpandableEntry key={item.name} name={item.name} badge={item.kind} summary={`${item.global ? "All workspaces" : "Private"} · Owner: ${item.owner} · ${item.ready ? "Ready" : "Needs retry"}`}>
    <p>Keys: {item.keys.join(", ")}</p><MappingSummary mapping={item.mapping}/>
    {(user.role === "admin" || (item.owner === user.username && !item.global)) && <>
     <ConfigurationEditor item={item} user={user} api={api} action={action} busy={busy}/>
     <p className="intro-copy">Disable global mapping and detach from workspaces, then wait for old pods to terminate before deletion. Disabling global mapping restarts all affected workspaces.</p>
     <button className="secondary-button" disabled={busy || item.global} onClick={() => { if (confirm(`Delete ${item.kind} ${item.name}?`)) action(() => api(`configurations/${item.name}`, "DELETE")); }}>Delete configuration</button>
    </>}
   </ExpandableEntry>)}
  </section>
 </section>;
}

export function WorkspaceConfigurations({ item, configurations, api, action, busy }) {
 const [selected, setSelected] = useState(""), [mapping, setMapping] = useState(emptyMapping("")), [editing, setEditing] = useState(false);
 const mounted = item.configurations || [], globals = configurations.filter(c => c.global);
 const available = privateConfigurations(configurations, item.owner);
 const config = available.find(c => c.name === selected);
 function choose(name) {
  setSelected(name); setEditing(true);
  setMapping(mounted.find(m => m.configuration === name) || available.find(c => c.name === name)?.mapping || emptyMapping(name));
 }
 return <section><h4>ConfigMaps and Secrets</h4>
  {globals.map(c => <div key={c.name}><strong>{c.name} (admin global mapping)</strong><MappingSummary mapping={c.mapping}/></div>)}
  {mounted.map(m => <div key={m.configuration}><strong>{m.configuration}</strong><MappingSummary mapping={m}/><button className="secondary-button" disabled={busy} onClick={() => choose(m.configuration)}>Edit mapping</button> <button className="secondary-button" disabled={busy} onClick={() => action(() => api(`workspaces/${item.name}/configurations`, "PUT", { configurations: mounted.filter(other => other.configuration !== m.configuration) }))}>Detach</button></div>)}
  <form onSubmit={e => { e.preventDefault(); action(async () => { await api(`workspaces/${item.name}/configurations`, "PUT", { configurations: [...mounted.filter(m => m.configuration !== selected), { ...mapping, configuration: selected }] }); setSelected(""); setEditing(false); }); }}>
   <label>Private configuration<select required disabled={busy} value={selected} onChange={e => choose(e.target.value)}><option value="">Select a configuration</option>{available.map(c => <option key={c.name} value={c.name}>{c.name} ({c.kind})</option>)}</select></label>
   {editing && config && <MappingFields value={mapping} onChange={setMapping} keys={config.keys} disabled={busy}/>}
   <p className="intro-copy">Create private configurations in ConfigMaps &amp; Secrets. Mappings are retained when undeployed. Global mappings cannot be overridden. Detach private configurations before transferring ownership; copied secrets in the home disk are not erased.</p>
   <button className="secondary-button" disabled={busy || !config}>Save workspace mapping</button>
  </form>
 </section>;
}

// Controlled fields inside the create-workspace form, deliberately not a form
// of their own: all attachments are submitted in the initial workspace POST.
export function CreateWorkspaceConfigurations({ owner, configurations, value, onChange, busy }) {
 const available = privateConfigurations(configurations, owner);
 const globals = configurations.filter(item => item.global);
 function toggle(item, checked) {
  onChange(checked ? [...value, initialConfigurationMapping(item)] : value.filter(mapping => mapping.configuration !== item.name));
 }
 return <section>
  <h3>ConfigMaps and Secrets</h3>
  <p className="intro-copy">Select existing private entries owned by {owner || "the workspace owner"}. Edit file paths and environment variables before creating the workspace; attachments are applied on its first start.</p>
  {globals.map(item => <div key={item.name}><strong>{item.name} ({item.kind}, automatic admin global mapping)</strong><MappingSummary mapping={item.mapping}/>{!item.ready && <p role="alert">This global configuration needs repair before workspace creation.</p>}</div>)}
  {available.length === 0 && <p className="intro-copy">No ready private configurations for this owner. Create entries in ConfigMaps &amp; Secrets first, or select a different owner. Global mappings apply automatically.</p>}
  {available.map(item => {
   const mapping = value.find(selected => selected.configuration === item.name);
   return <section key={item.name}>
    <label><input type="checkbox" checked={Boolean(mapping)} disabled={busy || (!mapping && value.length >= 16)} onChange={e => toggle(item, e.target.checked)}/> Attach {item.name} ({item.kind})</label>
    {mapping && <><MappingFields value={mapping} keys={item.keys} disabled={busy} onChange={next => onChange(value.map(selected => selected.configuration === item.name ? next : selected))}/>{(mapping.files || []).length + (mapping.env || []).length === 0 && <p role="alert">Add at least one file or environment mapping.</p>}</>}
   </section>;
  })}
  {value.filter(mapping => !available.some(item => item.name === mapping.configuration)).map(mapping => <p key={mapping.configuration} role="alert">{mapping.configuration} is no longer available for this owner. <button type="button" className="secondary-button" disabled={busy} onClick={() => onChange(value.filter(selected => selected.configuration !== mapping.configuration))}>Remove attachment</button></p>)}
 </section>;
}
