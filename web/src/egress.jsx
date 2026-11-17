import React, { useState } from "react";
import { egressDestination, emptyEgressDraft, egressDraftFromRule, updatedEgressRules } from "./egress.js";
import { Modal } from "./modal.jsx";

export function EgressEditor({settings,endpoint,api,action,busy,editable=true}) {
 const [draft,setDraft]=useState(emptyEgressDraft), [editing,setEditing]=useState(null);
 const [open,setOpen]=useState(false), [formError,setFormError]=useState(""), [saving,setSaving]=useState(false);
 const [message,setMessage]=useState("");
 const rules=settings?.rules||[], disabled=busy||!settings?.enabled;
 function field(name,value){setDraft({...draft,[name]:value});setFormError("");}
 function close(){setOpen(false);setEditing(null);setDraft(emptyEgressDraft());setFormError("");}
 function add(){setDraft(emptyEgressDraft());setEditing(null);setFormError("");setMessage("");setOpen(true);}
 async function save(event){
  event.preventDefault();if(saving||disabled)return;
  setSaving(true);setFormError("");
  try{await action(async()=>{
   try{
    await api(endpoint,"PUT",{rules:updatedEgressRules(rules,draft,editing)});
    close();setMessage("Rules applied without restarting the workspace.");
   }catch(error){setFormError(error.message);throw error;}
  });}finally{setSaving(false);}
 }
 function edit(rule){setEditing(rule.name);setMessage("");setFormError("");setDraft(egressDraftFromRule(rule));setOpen(true);}
 return <section className="egress-rules" aria-label="Outbound network rules">
  <p className="intro-copy">Additional outbound permissions. Existing DNS/internet permissions and admin defaults remain in effect. Rules do not change inbound access or restart pods.</p>
  {!settings?.enabled&&<p role="status">Network policies are disabled by the administrator.</p>}
  {settings?.enabled&&settings?.status!=="ready"&&<p role="status">Reconciliation: {settings?.status||"pending"}. Saved rules retry automatically.</p>}
  {settings?.error&&<p role="alert" className="error-banner">{settings.error}</p>}
  {editable&&settings?.status==="error"&&<button type="button" className="secondary-button" disabled={disabled} onClick={()=>action(async()=>{await api(endpoint,"PUT",{rules});setMessage("Saved rules reapplied.");})}>Retry saved rules</button>}
  {rules.length===0&&<p className="intro-copy">No additional rules.</p>}
  <ul className="ssh-key-list">{rules.map(rule=><li key={rule.name}><div><strong>{rule.name}</strong><p><code>{egressDestination(rule)}</code> · {rule.protocol}/{rule.port}</p></div>{editable&&<div className="hub-actions"><button type="button" className="secondary-button" disabled={disabled} onClick={()=>edit(rule)}>Edit</button><button type="button" className="secondary-button" disabled={disabled} onClick={()=>{if(confirm(`Remove outbound rule ${rule.name}?`))action(async()=>{await api(endpoint,"PUT",{rules:rules.filter(item=>item.name!==rule.name)});if(editing===rule.name)close();setMessage("Rule removed. Other policies may still allow this destination.");});}}>Remove</button></div>}</li>)}</ul>
  {editable&&<div className="hub-actions"><button type="button" className="secondary-button" disabled={disabled||rules.length>=32} onClick={add}>Add rule</button></div>}
  {editable&&open&&<Modal title={editing?`Edit outbound rule: ${editing}`:"Add outbound rule"} onClose={close} busy={saving}>
   <p className="intro-copy">{endpoint==="egress-defaults"?"This default rule applies to all existing and future workspaces.":`This rule applies only to workspace ${endpoint.split("/")[1]}.`}</p>
   {formError&&<p role="alert" className="error-banner">{formError}</p>}
   <form className="egress-rules" onSubmit={save}>
   <fieldset disabled={disabled||saving}>
    <label>Rule name<input data-autofocus required maxLength={40} pattern="[a-z]([a-z0-9-]*[a-z0-9])?" value={draft.name} onChange={e=>field("name",e.target.value)} placeholder="litellm-https"/></label>
    <label>Destination type<select value={draft.type} onChange={e=>field("type",e.target.value)}><option value="cidr">IP address / CIDR</option><option value="pods">Cluster pods (namespace + labels)</option></select></label>
    {draft.type==="cidr"?<label>Destination IP / CIDR<input required maxLength={64} value={draft.cidr} onChange={e=>field("cidr",e.target.value)} placeholder="192.0.2.10/32"/></label>:<><label>Destination namespace<input required maxLength={63} value={draft.namespace} onChange={e=>field("namespace",e.target.value)} placeholder="traefik"/></label><label>Destination pod labels<textarea required rows={3} value={draft.labels} onChange={e=>field("labels",e.target.value)} placeholder="app.kubernetes.io/name=traefik"/></label><p className="intro-copy">One key=value per line. All labels must match, within this namespace only.</p></>}
    <label>Protocol<select value={draft.protocol} onChange={e=>field("protocol",e.target.value)}><option>TCP</option><option>UDP</option></select></label>
    <label>Destination port<input required type="number" min={1} max={65535} step={1} value={draft.port} onChange={e=>field("port",e.target.value)}/></label>
    <p className="intro-copy">Use numeric IPs, not hostnames. Load balancers may require a second rule for the translated ingress pods and backend port (for example Traefik TCP/8443). Allowing an ingress proxy permits other routes served by that proxy too. Maximum 32 rules.</p>
   </fieldset>
    <div className="hub-actions"><button className="primary-button" disabled={disabled||saving||(!editing&&rules.length>=32)}>{saving?"Saving…":editing?"Save rule":"Add rule"}</button><button type="button" className="secondary-button" disabled={saving} onClick={close}>Cancel</button></div>
   </form>
  </Modal>}
  {message&&<p role="status" className="intro-copy">{message}</p>}
 </section>;
}

export function WorkspaceEgress({item,...props}) {
 return <section><h4>Outbound network access</h4><p className="intro-copy">These exceptions apply only to {item.name}. Admin defaults also apply; view them under Network access.</p><EgressEditor settings={item.egress} endpoint={`workspaces/${item.name}/egress`} {...props} busy={props.busy||["deleting","undeploying"].includes(item.status)}/></section>;
}
