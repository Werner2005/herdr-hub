export function parsePodLabels(text) {
 const labels = {};
 for (const line of text.split("\n").map(value=>value.trim()).filter(Boolean)) {
  const index = line.indexOf("=");
  if(index<1)throw new Error("Pod labels must use key=value, one per line.");
  const key=line.slice(0,index).trim(), value=line.slice(index+1).trim();
  if(Object.hasOwn(labels,key))throw new Error(`Duplicate pod label: ${key}`);
  Object.defineProperty(labels,key,{value,enumerable:true,writable:true,configurable:true});
 }
 if(!Object.keys(labels).length)throw new Error("Provide at least one pod label.");
 return labels;
}

export function egressDestination(rule) {
 return rule.cidr || `${rule.namespace}: ${Object.entries(rule.pod_selector||{}).map(([key,value])=>`${key}=${value}`).join(", ")}`;
}

export function egressRuleInput(draft) {
 const rule={name:draft.name,protocol:draft.protocol,port:Number(draft.port)};
 return draft.type==="pods"?{...rule,namespace:draft.namespace,pod_selector:parsePodLabels(draft.labels)}:{...rule,cidr:draft.cidr.trim()};
}

export function emptyEgressDraft() {
 return {name:"",type:"cidr",cidr:"",namespace:"",labels:"",protocol:"TCP",port:"443"};
}

export function egressDraftFromRule(rule) {
 return {name:rule.name,type:rule.cidr?"cidr":"pods",cidr:rule.cidr||"",namespace:rule.namespace||"",labels:Object.entries(rule.pod_selector||{}).map(([key,value])=>`${key}=${value}`).join("\n"),protocol:rule.protocol,port:String(rule.port)};
}

export function updatedEgressRules(rules,draft,editing) {
 const rule=egressRuleInput(draft);
 if(editing&&!rules.some(item=>item.name===editing))throw new Error("This rule was removed. Cancel the edit and refresh before adding it again.");
 if(rules.some(item=>item.name===rule.name&&item.name!==editing))throw new Error("Rule name already exists.");
 return editing?rules.map(item=>item.name===editing?rule:item):[...rules,rule];
}
