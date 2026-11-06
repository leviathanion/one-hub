import PropTypes from 'prop-types';
import { useDeferredValue, useEffect, useMemo, useRef, useState } from 'react';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Checkbox from '@mui/material/Checkbox';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import LinearProgress from '@mui/material/LinearProgress';
import MenuItem from '@mui/material/MenuItem';
import Stack from '@mui/material/Stack';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TablePagination from '@mui/material/TablePagination';
import TableRow from '@mui/material/TableRow';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { API } from 'utils/api';
import { showSuccess } from 'utils/common';
import OwnedByEditModal from 'views/ModelOwnedby/component/EditModal';
import { buildOwnershipBatch, CLEAR_OWNERSHIP, MAX_OWNERSHIP_BATCH, parseOwnershipModels } from './modelOwnership.mjs';

const PAGE_SIZE_OPTIONS = [25, 50, 100, 200];
const requestOptions = (signal) => ({ signal, skipErrorNotification: true });

function responseData(response) {
  if (!response.data.success) throw new Error(response.data.message || '操作失败，请重试');
  return response.data.data;
}

function sourceDescription(suggestion) {
  if (!suggestion) return '尚未查询';
  if (suggestion.status === 'unmatched') return '未查到服务提供方';
  const providers = suggestion.providers.map((provider) => `${provider.name}（${provider.id}）`).join('、');
  if (suggestion.status === 'conflict') return `提供方冲突：${providers}`;
  return providers;
}

// 关闭后卸载编辑器，终止只读请求并丢弃本次选择；下一次打开重新加载目录。
export default function BatchOwnershipDialog({ open, method, onClose, onSaved }) {
  return open ? <OwnershipEditor key={method} method={method} onClose={onClose} onSaved={onSaved} /> : null;
}

function OwnershipEditor({ method, onClose, onSaved }) {
  const [catalog, setCatalog] = useState([]);
  const [owners, setOwners] = useState({});
  const [selected, setSelected] = useState(() => new Set());
  const [keyword, setKeyword] = useState('');
  const query = useDeferredValue(keyword.trim().toLowerCase());
  const [ownerFilter, setOwnerFilter] = useState('missing');
  const [onlySelected, setOnlySelected] = useState(false);
  const [page, setPage] = useState(0);
  const [rowsPerPage, setRowsPerPage] = useState(25);
  const [pasted, setPasted] = useState('');
  const [target, setTarget] = useState('');
  const [mode, setMode] = useState('fill_missing');
  const [suggestions, setSuggestions] = useState(() => Object.create(null));
  const [providerOwners, setProviderOwners] = useState(() => Object.create(null));
  const [loading, setLoading] = useState(true);
  const [loaded, setLoaded] = useState(false);
  const [matching, setMatching] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [reload, setReload] = useState(0);
  const [createOwner, setCreateOwner] = useState(false);
  const mounted = useRef(false);
  const suggestionRequest = useRef(null);
  const ownerRequest = useRef(null);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      suggestionRequest.current?.abort();
      ownerRequest.current?.abort();
    };
  }, []);

  useEffect(() => {
    const request = new AbortController();
    setLoading(true);
    setError('');
    Promise.all([
      API.get('/api/model_info/ownership/catalog', requestOptions(request.signal)),
      API.get('/api/ownedby', requestOptions(request.signal))
    ])
      .then(([models, dictionary]) => {
        if (request.signal.aborted) return;
        setCatalog(responseData(models));
        setOwners(responseData(dictionary));
        setLoaded(true);
      })
      .catch((err) => {
        if (!request.signal.aborted) setError(err.message);
      })
      .finally(() => {
        if (!request.signal.aborted) setLoading(false);
      });
    return () => request.abort();
  }, [reload]);

  const filtered = useMemo(
    () =>
      catalog.filter(
        (row) =>
          (!query || row.model.toLowerCase().includes(query) || (row.name || '').toLowerCase().includes(query)) &&
          (ownerFilter === 'all' || (ownerFilter === 'missing' ? row.owned_by_id == null : String(row.owned_by_id) === ownerFilter)) &&
          (!onlySelected || selected.has(row.model))
      ),
    [catalog, query, ownerFilter, onlySelected, selected]
  );
  const currentPage = Math.min(page, Math.max(0, Math.ceil(filtered.length / rowsPerPage) - 1));
  const pageRows = filtered.slice(currentPage * rowsPerPage, (currentPage + 1) * rowsPerPage);
  const selectedOnPage = pageRows.filter((row) => selected.has(row.model)).length;
  const allPageSelected = pageRows.length > 0 && selectedOnPage === pageRows.length;
  const ownerOptions = useMemo(() => Object.entries(owners).sort(([a], [b]) => Number(a) - Number(b)), [owners]);
  const sourceGroups = useMemo(() => {
    const groups = new Map();
    for (const model of selected) {
      const suggestion = suggestions[model];
      if (suggestion?.status !== 'matched' || suggestion.providers.length !== 1) continue;
      const provider = suggestion.providers[0];
      if (!groups.has(provider.id)) groups.set(provider.id, { provider, models: [] });
      groups.get(provider.id).models.push(model);
    }
    return [...groups].sort(([a], [b]) => a.localeCompare(b));
  }, [selected, suggestions]);
  const batch = useMemo(
    () => buildOwnershipBatch({ selected, method, target, mode, suggestions, providerOwners, owners }),
    [selected, method, target, mode, suggestions, providerOwners, owners]
  );
  const selectedNewCount = catalog.filter((row) => selected.has(row.model) && !row.has_info).length;

  const updateSelection = (models, remove) => {
    const next = new Set(selected);
    for (const model of models) {
      if (remove) next.delete(model);
      else next.add(model);
    }
    if (next.size > MAX_OWNERSHIP_BATCH) {
      setError(`每次最多选择 ${MAX_OWNERSHIP_BATCH} 个模型，请缩小范围`);
      return false;
    }
    setSelected(next);
    setError('');
    return true;
  };

  const addPasted = () => {
    try {
      const models = parseOwnershipModels(pasted);
      if (!updateSelection(models, false)) return;
      setCatalog((current) => {
        const existing = new Set(current.map((row) => row.model));
        const added = models
          .filter((model) => !existing.has(model))
          .map((model) => ({ model, name: '', owned_by_id: null, has_info: false }));
        return added.length ? [...current, ...added] : current;
      });
      setPasted('');
    } catch (err) {
      setError(err.message);
    }
  };

  const matchSource = async () => {
    suggestionRequest.current?.abort();
    const request = new AbortController();
    suggestionRequest.current = request;
    const models = [...selected];
    setMatching(true);
    setError('');
    try {
      const response = await API.post('/api/model_info/ownership/suggestions', { models }, requestOptions(request.signal));
      if (!request.signal.aborted) {
        setSuggestions(Object.assign(Object.create(null), Object.fromEntries(responseData(response).map((item) => [item.model, item]))));
      }
    } catch (err) {
      if (!request.signal.aborted) setError(err.message);
    } finally {
      if (!request.signal.aborted) setMatching(false);
    }
  };

  const reloadOwners = async () => {
    setCreateOwner(false);
    ownerRequest.current?.abort();
    const request = new AbortController();
    ownerRequest.current = request;
    try {
      const response = await API.get('/api/ownedby', requestOptions(request.signal));
      if (!request.signal.aborted) setOwners(responseData(response));
    } catch (err) {
      if (!request.signal.aborted) setError(`刷新归属选项失败：${err.message}`);
    }
  };

  const save = async () => {
    if (batch.error || saving) return;
    setSaving(true);
    setError('');
    try {
      const response = await API.post('/api/model_info/ownership/batch', { mode, items: batch.items }, { skipErrorNotification: true });
      const result = responseData(response);
      if (!mounted.current) return;
      showSuccess(`归属已保存：新增 ${result.created}，更新 ${result.updated}，未变化 ${result.unchanged}，跳过 ${result.skipped}`);
      onSaved(result);
    } catch (err) {
      if (mounted.current) setError(err.message);
    } finally {
      if (mounted.current) setSaving(false);
    }
  };

  const changeFilter = (setter, value) => {
    setter(value);
    setPage(0);
  };

  return (
    <>
      <Dialog open onClose={saving ? undefined : onClose} fullWidth maxWidth="lg" aria-labelledby="batch-ownership-title">
        <DialogTitle id="batch-ownership-title">{method === 'manual' ? '手动设置归属' : '从 models.dev 匹配归属'}</DialogTitle>
        <DialogContent dividers>
          <Stack spacing={2}>
            <Typography variant="body2" color="text.secondary">
              {method === 'manual'
                ? '选择模型后，将它们统一设置为本系统中的一个归属分类，也可以清空归属。'
                : '先选择模型并查询 models.dev，再将查到的服务提供方映射到本系统的归属分类。确认保存后才会修改所选模型的归属。'}
              {' 此操作只调整模型的展示归属，不修改价格或渠道。'}
            </Typography>
            {loading ? <LinearProgress aria-label="正在加载模型目录" /> : null}
            {error ? <Alert severity="error">{error}</Alert> : null}
            {!loading && !loaded ? <Button onClick={() => setReload((value) => value + 1)}>重新加载目录</Button> : null}
            <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2}>
              <TextField
                id="ownership-search"
                label="搜索模型"
                value={keyword}
                onChange={(event) => changeFilter(setKeyword, event.target.value)}
                size="small"
                fullWidth
                disabled={saving}
              />
              <TextField
                id="ownership-current-owner"
                select
                label="当前归属"
                value={ownerFilter}
                onChange={(event) => changeFilter(setOwnerFilter, event.target.value)}
                size="small"
                sx={{ minWidth: 180 }}
                disabled={saving}
              >
                <MenuItem value="all">全部归属</MenuItem>
                <MenuItem value="missing">未设置归属</MenuItem>
                {ownerOptions.map(([id, owner]) => (
                  <MenuItem key={id} value={id}>
                    {owner.name}
                  </MenuItem>
                ))}
              </TextField>
            </Stack>
            <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1} alignItems={{ sm: 'center' }}>
              <Typography variant="body2" role="status">
                已选 {selected.size} / {MAX_OWNERSHIP_BATCH} 个，其中 {selected.size - selectedOnPage} 个不在当前页
              </Typography>
              <FormControlLabel
                control={
                  <Checkbox
                    checked={onlySelected}
                    onChange={(event) => {
                      changeFilter(setOnlySelected, event.target.checked);
                      if (event.target.checked) {
                        setKeyword('');
                        setOwnerFilter('all');
                      }
                    }}
                    disabled={saving}
                  />
                }
                label="只看已选"
              />
              <Button onClick={() => setSelected(new Set())} disabled={saving || !selected.size}>
                清空选择
              </Button>
            </Stack>
            <TableContainer sx={{ maxHeight: 380 }}>
              <Table size="small" stickyHeader aria-label="模型归属候选">
                <TableHead>
                  <TableRow>
                    <TableCell padding="checkbox">
                      <Checkbox
                        checked={allPageSelected}
                        indeterminate={selectedOnPage > 0 && !allPageSelected}
                        onChange={() =>
                          updateSelection(
                            pageRows.map((row) => row.model),
                            allPageSelected
                          )
                        }
                        disabled={saving || !loaded || !pageRows.length}
                        inputProps={{ 'aria-label': '选择本页模型' }}
                      />
                    </TableCell>
                    <TableCell>模型标识</TableCell>
                    <TableCell>当前归属</TableCell>
                    <TableCell>模型信息</TableCell>
                    {method === 'source' ? <TableCell>models.dev 服务提供方</TableCell> : null}
                  </TableRow>
                </TableHead>
                <TableBody>
                  {pageRows.map((row) => (
                    <TableRow key={row.model} selected={selected.has(row.model)}>
                      <TableCell padding="checkbox">
                        <Checkbox
                          checked={selected.has(row.model)}
                          onChange={() => updateSelection([row.model], selected.has(row.model))}
                          disabled={saving || !loaded}
                          inputProps={{ 'aria-label': `选择 ${row.model}` }}
                        />
                      </TableCell>
                      <TableCell sx={{ overflowWrap: 'anywhere', maxWidth: 360 }}>
                        {row.model}
                        {row.name && row.name !== row.model ? (
                          <Typography variant="caption" display="block" color="text.secondary">
                            {row.name}
                          </Typography>
                        ) : null}
                      </TableCell>
                      <TableCell>
                        {row.owned_by_id == null ? '未设置' : owners[row.owned_by_id]?.name || `归属 ${row.owned_by_id}`}
                      </TableCell>
                      <TableCell>{row.has_info ? '已存在' : '尚无模型信息'}</TableCell>
                      {method === 'source' ? (
                        <TableCell sx={{ overflowWrap: 'anywhere', maxWidth: 220 }}>{sourceDescription(suggestions[row.model])}</TableCell>
                      ) : null}
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
            {!filtered.length && loaded ? (
              <Typography color="text.secondary">没有符合条件的模型，也可以在下方粘贴精确模型名。</Typography>
            ) : null}
            <TablePagination
              component="div"
              sx={{
                '& .MuiTablePagination-toolbar': { flexWrap: 'wrap' },
                '& .MuiTablePagination-spacer': { display: { xs: 'none', sm: 'block' } }
              }}
              count={filtered.length}
              page={currentPage}
              rowsPerPage={rowsPerPage}
              rowsPerPageOptions={PAGE_SIZE_OPTIONS}
              onRowsPerPageChange={(event) => {
                setRowsPerPage(Number(event.target.value));
                setPage(0);
              }}
              onPageChange={(_, next) => setPage(next)}
              labelRowsPerPage="每页模型数"
              labelDisplayedRows={({ from, to, count }) => `${from}–${to} / ${count}`}
              getItemAriaLabel={(type) => (type === 'next' ? '下一页模型' : '上一页模型')}
            />
            <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1} alignItems={{ sm: 'center' }}>
              <TextField
                id="ownership-pasted-models"
                label="粘贴模型名（每行一个）"
                value={pasted}
                onChange={(event) => setPasted(event.target.value)}
                multiline
                minRows={2}
                maxRows={4}
                fullWidth
                disabled={saving}
              />
              <Button variant="outlined" onClick={addPasted} disabled={saving || !loaded || !pasted.trim()} sx={{ flexShrink: 0 }}>
                加入选择
              </Button>
            </Stack>
            <TextField
              id="ownership-mode"
              select
              label="已有归属处理"
              value={mode}
              onChange={(event) => setMode(event.target.value)}
              size="small"
              fullWidth
              disabled={saving}
            >
              <MenuItem value="fill_missing" disabled={method === 'manual' && target === CLEAR_OWNERSHIP}>
                仅补未设置归属
              </MenuItem>
              <MenuItem value="replace">覆盖所选模型的归属</MenuItem>
            </TextField>
            {method === 'manual' ? (
              <TextField
                id="ownership-target"
                select
                label="目标归属"
                value={target}
                size="small"
                fullWidth
                disabled={saving}
                onChange={(event) => {
                  setTarget(event.target.value);
                  if (event.target.value === CLEAR_OWNERSHIP) setMode('replace');
                }}
              >
                <MenuItem value="" disabled>
                  请选择归属
                </MenuItem>
                {ownerOptions.map(([id, owner]) => (
                  <MenuItem key={id} value={id}>
                    {owner.name}
                  </MenuItem>
                ))}
                <MenuItem value={CLEAR_OWNERSHIP}>清空归属</MenuItem>
              </TextField>
            ) : (
              <Stack spacing={2}>
                <Box>
                  <Button variant="outlined" onClick={matchSource} disabled={saving || matching || !loaded || !selected.size}>
                    {matching ? '正在查询…' : '查询所选模型的归属'}
                  </Button>
                </Box>
                <Typography variant="body2" color="text.secondary">
                  左列是 models.dev 记录的 API 服务提供方。例如，Cloudflare 托管的 Kimi、Nemotron 模型都会列在 Cloudflare Workers AI 下。
                  右列请选择对应的本系统归属，每行选择会应用到该行列出的所有模型；名称相同也需要确认。
                  未查到服务提供方或存在多个提供方的模型，请先从上方列表取消选择，再保存其他模型。
                </Typography>
                {sourceGroups.length > 0 ? (
                  <TableContainer>
                    <Table
                      size="small"
                      aria-label="models.dev 归属映射"
                      sx={{
                        tableLayout: 'fixed',
                        '& .MuiTableCell-root': { whiteSpace: 'normal', overflowWrap: 'anywhere', px: { xs: 1, sm: 2 } }
                      }}
                    >
                      <TableHead>
                        <TableRow>
                          <TableCell sx={{ width: '45%' }}>models.dev 服务提供方</TableCell>
                          <TableCell>映射到本系统的归属</TableCell>
                        </TableRow>
                      </TableHead>
                      <TableBody>
                        {sourceGroups.map(([providerID, { provider, models }], index) => (
                          <TableRow key={providerID}>
                            <TableCell component="th" scope="row" sx={{ verticalAlign: 'top', overflowWrap: 'anywhere' }}>
                              <Typography variant="subtitle2">{provider.name}</Typography>
                              {provider.name !== providerID ? (
                                <Typography variant="caption" color="text.secondary">
                                  {providerID}
                                </Typography>
                              ) : null}
                              <Box component="details" sx={{ mt: 0.5 }}>
                                <Box component="summary" sx={{ cursor: 'pointer', typography: 'body2' }}>
                                  {models.length} 个模型，查看列表
                                </Box>
                                <Box component="ul" sx={{ pl: 2, mb: 0, maxHeight: 160, overflowY: 'auto' }}>
                                  {models.map((model) => (
                                    <li key={model}>{model}</li>
                                  ))}
                                </Box>
                              </Box>
                            </TableCell>
                            <TableCell sx={{ verticalAlign: 'top' }}>
                              <TextField
                                id={`ownership-provider-${index}`}
                                select
                                SelectProps={{
                                  displayEmpty: true,
                                  inputProps: { 'aria-label': `${provider.name}（${providerID}）映射到本系统的归属` }
                                }}
                                value={providerOwners[providerID] || ''}
                                size="small"
                                fullWidth
                                sx={{ '& .MuiSelect-select': { whiteSpace: 'normal', overflowWrap: 'anywhere' } }}
                                disabled={saving || matching}
                                onChange={(event) =>
                                  setProviderOwners((current) =>
                                    Object.assign(Object.create(null), current, { [providerID]: event.target.value })
                                  )
                                }
                              >
                                <MenuItem value="">请选择本系统归属</MenuItem>
                                {ownerOptions.map(([id, owner]) => (
                                  <MenuItem key={id} value={id}>
                                    {owner.name}
                                  </MenuItem>
                                ))}
                              </TextField>
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  </TableContainer>
                ) : null}
              </Stack>
            )}
            <Box>
              <Button onClick={() => setCreateOwner(true)} disabled={saving || loading || !loaded}>
                新建归属分类
              </Button>
              <Button onClick={reloadOwners} disabled={saving || loading || !loaded}>
                刷新归属选项
              </Button>
            </Box>
            {selected.size > 0 ? (
              <Typography variant="body2">
                将处理 {selected.size} 个模型，其中 {selectedNewCount} 个尚无模型信息。
                {mode === 'fill_missing' ? '保存时已有归属的模型会跳过。' : '所选模型的已有归属会被替换。'}
              </Typography>
            ) : null}
            {selected.size > 0 && batch.error && !matching ? <Alert severity="info">{batch.error}</Alert> : null}
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button onClick={onClose} disabled={saving}>
            取消
          </Button>
          <Button variant="contained" onClick={save} disabled={!loaded || loading || matching || saving || Boolean(batch.error)}>
            {saving
              ? '正在保存…'
              : method === 'manual' && target === CLEAR_OWNERSHIP
                ? `清空 ${selected.size} 个模型的归属`
                : `保存 ${selected.size} 个模型的归属`}
          </Button>
        </DialogActions>
      </Dialog>
      {createOwner ? (
        <OwnedByEditModal
          open
          Oid={0}
          onCancel={() => setCreateOwner(false)}
          onOk={(success) => {
            if (success) reloadOwners();
          }}
        />
      ) : null}
    </>
  );
}

BatchOwnershipDialog.propTypes = {
  open: PropTypes.bool.isRequired,
  method: PropTypes.oneOf(['manual', 'source']).isRequired,
  onClose: PropTypes.func.isRequired,
  onSaved: PropTypes.func.isRequired
};
OwnershipEditor.propTypes = {
  method: PropTypes.oneOf(['manual', 'source']).isRequired,
  onClose: PropTypes.func.isRequired,
  onSaved: PropTypes.func.isRequired
};
