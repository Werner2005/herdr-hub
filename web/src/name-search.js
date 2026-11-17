export function activeName(value, caret = value.length) {
 const before=value.slice(0,caret), after=value.slice(caret);
 const start=before.search(/[^\s,]*$/), end=caret+after.match(/^[^\s,]*/)[0].length;
 return {start,end,query:value.slice(start,end).toLowerCase(),selected:(value.slice(0,start)+" "+value.slice(end)).split(/[\s,]+/).filter(Boolean)};
}

export function insertName(value, token, name) {
 const suffix=token.end===value.length?", ":value.slice(token.end);
 return {value:value.slice(0,token.start)+name+suffix,caret:token.start+name.length+(token.end===value.length?2:0)};
}
