export function attachableVolumes(volumes, workspace) {
 const mounted = workspace.volumes || [];
 return volumes.filter(volume => !mounted.includes(volume.name) &&
  (volume.owner === workspace.owner || (volume.accessible_owners || []).includes(workspace.owner)));
}
