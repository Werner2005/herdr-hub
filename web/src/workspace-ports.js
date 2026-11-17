export function validPortDraft(name, protocol, port, mappings = []) {
 const number = Number(port);
 return /^[a-z]([a-z0-9-]*[a-z0-9])?$/.test(name) && name.length <= 40 &&
  ["HTTP", "TCP", "UDP"].includes(protocol) && Number.isInteger(number) &&
  number >= 1 && number <= 65535 && ![2222, 7317].includes(number) &&
  mappings.length < 16 && !mappings.some(mapping => mapping.name === name);
}
