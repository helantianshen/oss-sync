/** 传输通知；path 用于单文件提示，count 用于聚合提示 */
export type TransferNotice =
  | { readonly kind: "uploaded"; readonly path?: string; readonly count?: number }
  | { readonly kind: "downloaded"; readonly path?: string; readonly count?: number }
  | { readonly kind: "deltaSynced"; readonly path?: string; readonly count?: number }
  | { readonly kind: "failed"; readonly path: string };

/** 传输数量超过阈值时合并成功提示，失败仍逐条显示。 */
export function buildTransferNoticePlan(
  uploaded: readonly string[],
  downloaded: readonly string[],
  deltaSynced: readonly string[],
  failed: readonly string[],
  aggregateThreshold = 5,
): readonly TransferNotice[] {
  const notices: TransferNotice[] = [];
  const aggregate = uploaded.length + downloaded.length + deltaSynced.length + failed.length > aggregateThreshold;
  appendTransferNotices(notices, "uploaded", uploaded, aggregate);
  appendTransferNotices(notices, "downloaded", downloaded, aggregate);
  appendTransferNotices(notices, "deltaSynced", deltaSynced, aggregate);
  for (const path of failed) notices.push({ kind: "failed", path });
  return notices;
}

function appendTransferNotices(
  notices: TransferNotice[],
  kind: "uploaded" | "downloaded" | "deltaSynced",
  paths: readonly string[],
  aggregate: boolean,
): void {
  if (paths.length === 0) return;
  if (aggregate) {
    notices.push({ kind, count: paths.length });
    return;
  }
  for (const path of paths) notices.push({ kind, path });
}
