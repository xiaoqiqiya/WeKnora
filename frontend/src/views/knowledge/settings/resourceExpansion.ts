import type { Resource } from '../../../api/datasource'

// Re-read the tree after each level because expanding a lazy node adds new nodes.
export async function expandResourceTree(
  resources: () => readonly Pick<Resource, 'external_id' | 'has_children'>[],
  load: (id: string) => Promise<void>,
  cancelled: () => boolean,
): Promise<void> {
  const visited = new Set<string>()
  while (!cancelled()) {
    const next = resources().filter(r => r.has_children && !visited.has(r.external_id))
    if (next.length === 0) return
    for (const resource of next) {
      if (cancelled()) return
      if (visited.has(resource.external_id)) continue
      visited.add(resource.external_id)
      await load(resource.external_id)
    }
  }
}
