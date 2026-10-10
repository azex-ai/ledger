// A skeleton identifies layout positions, not business records. A row keeps
// its slot when the requested count changes; never use these IDs for data rows.
export function skeletonSlots(count: number, role: "row" | "header-cell" = "row") {
  return Array.from({ length: count }, (_, position) => ({ id: `${role}:${position + 1}` }));
}

export const fiveSkeletonRows = skeletonSlots(5);
