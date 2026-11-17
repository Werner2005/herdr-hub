export function privateConfigurations(configurations, owner) {
 return configurations.filter(item => !item.global && item.owner === owner && item.ready);
}

export function initialConfigurationMapping(item) {
 const files = (item.mapping?.files || []).map(file => ({ ...file }));
 const env = (item.mapping?.env || []).map(variable => ({ ...variable }));
 if (files.length + env.length === 0 && item.keys.length > 0) {
  files.push({ key: item.keys[0], path: `/mnt/${item.kind === "secret" ? "secrets" : "config"}/${item.name}/${item.keys[0]}` });
 }
 return { configuration: item.name, files, env };
}
