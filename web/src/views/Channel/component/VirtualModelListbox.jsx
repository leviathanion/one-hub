import { forwardRef, useCallback, useImperativeHandle, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { flushSync } from 'react-dom';
import PropTypes from 'prop-types';
import { Box, ListSubheader } from '@mui/material';
import { useForkRef } from '@mui/material/utils';
import { MODEL_ROW_HEIGHT, MODEL_VISIBLE_ROWS, modelNavigationTarget, modelWindow } from './modelSelection.mjs';

const OPTION_CHECK_SX = { width: 18, mr: 1, flexShrink: 0, color: 'primary.main', fontWeight: 700 };
// 固定行高使窗口无需测量；长名称在 title 和无障碍名称中保留全文。
// 若改为多行展示，需要同步替换窗口的行高计算。
const OPTION_LABEL_SX = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };

export function highlightedModelIndex(input) {
  const activeId = input?.getAttribute('aria-activedescendant');
  const prefix = `${input?.id}-option-`;
  return activeId?.startsWith(prefix) ? Number(activeId.slice(prefix.length)) : -1;
}

// renderOption/renderGroup 只传递行描述，复杂组件只在窗口内创建。
export const describeModelOption = (props, option) => ({ props, option });
export const describeModelGroup = (group) => group;

const VirtualModelListbox = forwardRef(function VirtualModelListbox(
  { children, inputRef, virtualRef, highlightedIdRef, resetKey, ...props },
  forwardedRef
) {
  const elementRef = useRef(null);
  const ref = useForkRef(elementRef, forwardedRef);
  const [scrollTop, setScrollTop] = useState(0);
  const [active, setActive] = useState({ index: -1, keyboard: false });
  const { rows, optionRows } = useMemo(() => {
    const rows = [];
    const optionRows = [];
    for (const group of children) {
      rows.push({ group: group.group, key: `group-${group.key}` });
      for (const item of group.children) {
        optionRows[item.props['data-option-index']] = rows.length;
        rows.push(item);
      }
    }
    return { rows, optionRows };
  }, [children]);

  const scrollToOption = useCallback(
    (index) => {
      const element = elementRef.current;
      if (!element) return;
      let nextTop = 0;
      if (index !== -1) {
        const row = optionRows[index];
        if (row === undefined) return;
        const top = row * MODEL_ROW_HEIGHT;
        nextTop = Math.min(element.scrollTop, top);
        if (top + MODEL_ROW_HEIGHT > nextTop + element.clientHeight) nextTop = top + MODEL_ROW_HEIGHT - element.clientHeight;
      }
      element.scrollTop = nextTop;
      setScrollTop(element.scrollTop);
    },
    [optionRows]
  );

  const highlight = useCallback(
    (index, reason) => {
      setActive((previous) => {
        const keyboard = reason === 'keyboard';
        return previous.index === index && previous.keyboard === keyboard ? previous : { index, keyboard };
      });
      if (reason !== 'mouse' && reason !== 'touch') scrollToOption(index);
    },
    [scrollToOption]
  );

  useImperativeHandle(
    virtualRef,
    () => ({
      highlight,
      prepareKeyDown(event) {
        const input = inputRef.current;
        const index = modelNavigationTarget(event.key, highlightedModelIndex(input), optionRows.length, input?.value);
        if (index === -1) return;
        // MUI 在自己的 keydown 中查询 DOM；跨窗口时先提交目标行，避免跳过未挂载选项。
        if (!elementRef.current?.querySelector(`[data-option-index="${index}"]`)) {
          flushSync(() => scrollToOption(index));
        }
      }
    }),
    [highlight, inputRef, optionRows.length, scrollToOption]
  );

  useLayoutEffect(() => {
    const input = inputRef.current;
    let index = highlightedModelIndex(input);
    // MUI 在重新打开且高亮项已选中时会提前返回，不重发 onHighlightChange。
    // 继承其最后通知的模型，恢复窗口及 aria 引用，不另行决定选择状态。
    if (index === -1 && highlightedIdRef.current !== null) {
      const previous = rows.find((row) => row.option?.id === highlightedIdRef.current);
      if (previous) {
        index = previous.props['data-option-index'];
        input?.setAttribute('aria-activedescendant', previous.props.id);
      }
    }
    highlight(index, 'auto');
    // 查询变化时重置窗口，并接住 MUI 在初次挂载 ref 时确定的高亮。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [resetKey]);

  useLayoutEffect(() => {
    // 滚动会重新挂载行，恢复 MUI 的高亮类；保留 active 行使 aria-activedescendant 始终有效。
    const index = highlightedModelIndex(inputRef.current);
    const option = elementRef.current?.querySelector(`[data-option-index="${index}"]`);
    option?.classList.add('Mui-focused');
    if (active.keyboard) option?.classList.add('Mui-focusVisible');
  });

  const { start, end } = modelWindow(scrollTop, rows.length);
  const visibleRows = Array.from({ length: end - start }, (_, i) => start + i);
  const activeRow = optionRows[active.index];
  if (activeRow !== undefined) {
    if (activeRow < start) visibleRows.unshift(activeRow);
    else if (activeRow >= end) visibleRows.push(activeRow);
  }

  return (
    <div
      {...props}
      ref={ref}
      onScroll={(event) => setScrollTop(event.currentTarget.scrollTop)}
      style={{
        ...props.style,
        padding: 0,
        position: 'relative',
        height: Math.min(rows.length, MODEL_VISIBLE_ROWS) * MODEL_ROW_HEIGHT,
        maxHeight: '40vh',
        overflow: 'auto'
      }}
    >
      <ul
        role="presentation"
        style={{ position: 'relative', height: rows.length * MODEL_ROW_HEIGHT, margin: 0, padding: 0, listStyle: 'none' }}
      >
        {visibleRows.map((rowIndex) => {
          const row = rows[rowIndex];
          const style = {
            position: 'absolute',
            top: rowIndex * MODEL_ROW_HEIGHT,
            height: MODEL_ROW_HEIGHT,
            width: '100%',
            boxSizing: 'border-box'
          };
          if (!row.option) {
            return (
              <ListSubheader key={row.key} component="li" role="presentation" disableSticky style={style}>
                {row.group}
              </ListSubheader>
            );
          }
          const { key, ...optionProps } = row.props;
          const index = optionProps['data-option-index'];
          return (
            <li
              {...optionProps}
              key={key}
              style={style}
              aria-posinset={index + 1}
              aria-setsize={optionRows.length}
              aria-label={row.option.group ? `${row.option.group}: ${row.option.id}` : row.option.id}
              title={row.option.id}
            >
              <Box component="span" aria-hidden sx={OPTION_CHECK_SX}>
                {optionProps['aria-selected'] ? '✓' : ''}
              </Box>
              <Box component="span" sx={OPTION_LABEL_SX}>
                {row.option.id}
              </Box>
            </li>
          );
        })}
      </ul>
    </div>
  );
});

VirtualModelListbox.propTypes = {
  children: PropTypes.array.isRequired,
  inputRef: PropTypes.object.isRequired,
  virtualRef: PropTypes.object.isRequired,
  highlightedIdRef: PropTypes.object.isRequired,
  resetKey: PropTypes.string.isRequired,
  style: PropTypes.object
};

export default VirtualModelListbox;
