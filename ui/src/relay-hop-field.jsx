import React, { useEffect, useId, useRef, useState } from "react";
import { ArrowDown, ArrowUp, Check, ChevronDown, Layers, X } from "lucide-react";
import { useTranslation } from "./interface-language.mjs";
import {
  MAX_RELAY_NODES,
  RELAY_STRATEGY_FALLBACK,
  RELAY_STRATEGY_URL_TEST,
  hopCandidateIDs,
  isMultiNodeHop,
  sourceUsableRelayCount,
} from "./proxy-route.mjs";

export function RelayHopField({
  id,
  label,
  hop,
  hopIndex,
  route,
  sources,
  usableIDs,
  claimedNodes,
  usedOtherSources,
  liveActiveID,
  routeName,
  sourceName,
  disabled,
  onSelectSource,
  onToggleNode,
  onMoveNode,
  onClear,
  onStrategyChange,
}) {
  const tr = useTranslation();
  const listID = useId();
  const rootRef = useRef(null);
  const triggerRef = useRef(null);
  const [open, setOpen] = useState(false);
  const [expanded, setExpanded] = useState(false);

  useEffect(() => {
    if (!open) return;
    const onOutside = (event) => {
      if (!rootRef.current?.contains(event.target)) {
        setOpen(false);
      }
    };
    document.addEventListener("click", onOutside, true);
    return () => document.removeEventListener("click", onOutside, true);
  }, [open]);

  const multiNode = isMultiNodeHop(hop);
  const isSourceMode = Boolean(hop.source_id);
  const isFallback = hop.strategy === RELAY_STRATEGY_FALLBACK;
  const candidateIDs = hopCandidateIDs(hop, route, sources, usableIDs);
  const selectedSet = new Set(isSourceMode ? candidateIDs : hop.node_ids || []);
  const eligibleSources = sources
    .map((src) => ({
      src,
      count: sourceUsableRelayCount(src, route, hopIndex, usableIDs, sources),
      dup: usedOtherSources.has(src.id),
    }))
    .filter(({ count, dup }) => !dup && count >= 2);

  const visibleNodeIDs = expanded ? hop.node_ids : hop.node_ids.slice(0, 6);
  const hiddenCount = hop.node_ids.length - visibleNodeIDs.length;

  const handleBoxClick = (event) => {
    if (disabled) return;
    if (event.target.closest("button")) return;
    setOpen((v) => !v);
  };

  const handleKeyDown = (event) => {
    if (disabled) return;
    if (event.key === "Escape" && open) {
      event.preventDefault();
      event.stopPropagation();
      setOpen(false);
      triggerRef.current?.focus();
    } else if ((event.key === "Enter" || event.key === " ") && event.target === triggerRef.current) {
      event.preventDefault();
      setOpen((v) => !v);
    }
  };

  return (
    <div ref={rootRef} className="proxy-hop-field" onKeyDown={handleKeyDown}>
      <div
        id={id}
        ref={triggerRef}
        role="combobox"
        tabIndex={disabled ? -1 : 0}
        aria-label={label}
        aria-expanded={open}
        aria-controls={open ? listID : undefined}
        aria-disabled={disabled}
        className={`proxy-hop-box${open ? " is-open" : ""}${disabled ? " is-disabled" : ""}`}
        onClick={handleBoxClick}
      >
        <div className="proxy-hop-tokens">
          {isSourceMode ? (
            <span className="proxy-hop-token is-source">
              <Layers size={12} aria-hidden="true" />
              <strong className="proxy-hop-token-name">{sourceName(hop.source_id)}</strong>
              <span className="proxy-hop-token-meta">{tr("整份订阅")}</span>
              <button
                type="button"
                className="proxy-hop-token-btn"
                disabled={disabled}
                aria-label={tr("移出候选池 {0}", sourceName(hop.source_id))}
                onClick={() => onClear()}
              >
                <X size={11} />
              </button>
            </span>
          ) : hop.node_ids.length === 0 ? (
            <span className="proxy-hop-placeholder">
              {tr("选择中转节点，或整份订阅（支持多选）")}
            </span>
          ) : (
            <>
              {visibleNodeIDs.map((nid, pos) => {
                const name = routeName(nid);
                const missing = !usableIDs.has(nid);
                const isLive = multiNode && liveActiveID === nid;
                const showOrder = multiNode && isFallback;
                return (
                  <span
                    key={nid}
                    className={`proxy-hop-token${isLive ? " is-live" : ""}${missing ? " is-missing" : ""}`}
                  >
                    {showOrder && (
                      <span className="proxy-hop-token-rank">
                        {pos === 0 ? tr("首选") : pos + 1}
                      </span>
                    )}
                    {isLive && <span className="proxy-relay-live-dot" aria-hidden="true" />}
                    <span className="proxy-hop-token-name" title={name}>{name}</span>
                    {showOrder && (
                      <>
                        <button
                          type="button"
                          className="proxy-hop-token-btn"
                          disabled={disabled || pos === 0}
                          aria-label={tr("上移 {0}", name)}
                          title={tr("提高优先级")}
                          onClick={() => onMoveNode(nid, -1)}
                        >
                          <ArrowUp size={11} />
                        </button>
                        <button
                          type="button"
                          className="proxy-hop-token-btn"
                          disabled={disabled || pos === hop.node_ids.length - 1}
                          aria-label={tr("下移 {0}", name)}
                          title={tr("降低优先级")}
                          onClick={() => onMoveNode(nid, 1)}
                        >
                          <ArrowDown size={11} />
                        </button>
                      </>
                    )}
                    <button
                      type="button"
                      className="proxy-hop-token-btn"
                      disabled={disabled}
                      aria-label={tr("移出候选池 {0}", name)}
                      title={tr("移出候选池")}
                      onClick={() => onToggleNode(nid)}
                    >
                      <X size={11} />
                    </button>
                  </span>
                );
              })}
              {hiddenCount > 0 && (
                <button
                  type="button"
                  className="proxy-hop-more-btn"
                  onClick={() => setExpanded(true)}
                >
                  {tr("+{0} 更多", hiddenCount)}
                </button>
              )}
              {expanded && hop.node_ids.length > 6 && (
                <button
                  type="button"
                  className="proxy-hop-more-btn"
                  onClick={() => setExpanded(false)}
                >
                  {tr("收起")}
                </button>
              )}
              {hop.node_ids.length === 1 && (
                <span className="proxy-hop-add-hint">{tr("+ 可继续添加备用")}</span>
              )}
            </>
          )}
        </div>
        <div className="proxy-hop-box-end">
          {hop.node_ids.length >= 2 && (
            <button
              type="button"
              className="proxy-hop-clear-btn"
              disabled={disabled}
              onClick={() => onClear()}
            >
              {tr("清空")}
            </button>
          )}
          <button
            type="button"
            className="proxy-hop-chevron-btn"
            tabIndex={-1}
            disabled={disabled}
            aria-label={tr("{0}选项", label)}
            onClick={() => setOpen((v) => !v)}
          >
            <ChevronDown size={14} aria-hidden="true" />
          </button>
        </div>
      </div>

      {open && !disabled && (
        <div
          id={listID}
          role="listbox"
          aria-multiselectable="true"
          aria-label={tr("{0}选项", label)}
          className="inline-select-list inline-select-list--inline proxy-hop-dropdown"
        >
          {eligibleSources.length > 0 && (
            <>
              <span className="inline-select-group" aria-hidden="true">
                {tr("整份订阅（自动切换）")}
              </span>
              {eligibleSources.map(({ src, count }) => {
                const selected = hop.source_id === src.id;
                const overCap = count > MAX_RELAY_NODES;
                return (
                  <span
                    key={`src:${src.id}`}
                    role="option"
                    aria-selected={selected}
                    aria-disabled={overCap}
                    className="proxy-hop-option is-source-option"
                    onClick={() => {
                      if (overCap) return;
                      onSelectSource(src.id);
                      setOpen(false);
                    }}
                  >
                    <span className="proxy-hop-option-main">
                      <Layers size={13} aria-hidden="true" />
                      <span dir="auto">{src.name}</span>
                    </span>
                    <span className="proxy-hop-option-tail">
                      <small>
                        {overCap
                          ? tr("超过 300 上限")
                          : tr("{0} 节点", count)}
                      </small>
                      {selected && <Check size={14} aria-hidden="true" />}
                    </span>
                  </span>
                );
              })}
            </>
          )}
          {sources.map((src) => (
            <React.Fragment key={src.id}>
              <span className="inline-select-group" aria-hidden="true">
                {src.name}
              </span>
              {src.nodes.map((n) => {
                const blocked = Boolean(n.blocked_reason);
                const claimed = claimedNodes.has(n.id);
                const selected = !blocked && selectedSet.has(n.id);
                const atCap = !selected && !isSourceMode && hop.node_ids.length >= MAX_RELAY_NODES;
                const itemDisabled = blocked || claimed || atCap;
                return (
                  <span
                    key={n.id}
                    role="option"
                    aria-selected={selected}
                    aria-disabled={itemDisabled}
                    className="proxy-hop-option"
                    onClick={() => {
                      if (itemDisabled) return;
                      const wasEmpty = !isSourceMode && hop.node_ids.length === 0;
                      onToggleNode(n.id);
                      if (wasEmpty) setOpen(false);
                    }}
                  >
                    <span className="proxy-hop-option-main">
                      <span className={`proxy-hop-option-check${selected ? " is-checked" : ""}`}>
                        {selected && <Check size={11} aria-hidden="true" />}
                      </span>
                      <span dir="auto">
                        {blocked ? tr("{0}（需迁移）", n.name) : n.name}
                      </span>
                    </span>
                  </span>
                );
              })}
            </React.Fragment>
          ))}
        </div>
      )}

      {multiNode && (
        <div className="proxy-hop-strategy-bar">
          <span className="proxy-hop-strategy-label">{tr("切换方式")}</span>
          <div className="proxy-hop-strategy-pills" role="group" aria-label={tr("切换方式")}>
            <button
              type="button"
              aria-pressed={hop.strategy === RELAY_STRATEGY_FALLBACK}
              disabled={disabled}
              onClick={() => onStrategyChange(RELAY_STRATEGY_FALLBACK)}
            >
              {tr("断线顺延")}
            </button>
            <button
              type="button"
              aria-pressed={hop.strategy === RELAY_STRATEGY_URL_TEST}
              disabled={disabled}
              onClick={() => onStrategyChange(RELAY_STRATEGY_URL_TEST)}
            >
              {tr("最低延迟")}
            </button>
          </div>
          {isSourceMode && liveActiveID && (
            <span className="proxy-relay-live">
              <span className="proxy-relay-live-dot" aria-hidden="true" />
              <span>{routeName(liveActiveID)}</span>
            </span>
          )}
        </div>
      )}
    </div>
  );
}
