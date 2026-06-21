import { useState } from 'react';
import { Grid, TextField, InputAdornment, Checkbox, Button, FormControlLabel, IconButton } from '@mui/material';
import { gridSpacing } from 'store/constant';
import { IconSearch, IconSend } from '@tabler/icons-react';
import { fetchChannelData } from '../index';
import { API } from 'utils/api';
import { showError, showSuccess } from 'utils/common';
import { useTranslation } from 'react-i18next';

export const azureBatchAPILabelOther = (other) => {
  const raw = String(other ?? '');
  try {
    const parsed = JSON.parse(raw);
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed) && typeof parsed.api_version === 'string') {
      return parsed.api_version;
    }
  } catch (error) {
    return raw;
  }
  return raw;
};

const BatchAzureAPI = () => {
  const [value, setValue] = useState('');
  const [data, setData] = useState([]);
  const [selected, setSelected] = useState([]);
  const [replaceValue, setReplaceValue] = useState('');
  const { t } = useTranslation();

  const handleSearch = async () => {
    const trimmed = value.trim();
    const data = await fetchChannelData(0, 100, { azure_api_version: trimmed, type: 3 }, 'desc', 'id');
    if (data) {
      setData(data.data);
    }
  };

  const handleSelect = (id) => {
    setSelected((prev) => {
      if (prev.includes(id)) {
        return prev.filter((i) => i !== id);
      } else {
        return [...prev, id];
      }
    });
  };

  const handleSelectAll = () => {
    if (selected.length === data.length) {
      setSelected([]);
    } else {
      setSelected(data.map((item) => item.id));
    }
  };

  const handleSubmit = async () => {
    try {
      const res = await API.put(`/api/channel/batch/azure_api`, {
        ids: selected,
        api_version: replaceValue.trim()
      });

      const { success, message, data } = res.data;
      if (success) {
        showSuccess(t('channel_index.batchAzureAPISuccess', { count: data }));
        return;
      } else {
        showError(message);
      }
    } catch (error) {
      console.error(error);
    }
  };

  return (
    <Grid container spacing={gridSpacing}>
      <Grid item xs={12}>
        <TextField
          sx={{ ml: 1, flex: 1 }}
          placeholder={t('channel_index.inputAPIVersion')}
          inputProps={{ 'aria-label': t('channel_index.inputAPIVersion') }}
          value={value}
          onChange={(e) => {
            setValue(e.target.value);
          }}
          InputProps={{
            endAdornment: (
              <InputAdornment position="end">
                <IconButton aria-label="toggle password visibility" onClick={handleSearch} edge="end">
                  <IconSearch />
                </IconButton>
              </InputAdornment>
            )
          }}
        />
      </Grid>
      {data.length === 0 ? (
        <Grid item xs={12}>
          {t('common.noData')}
        </Grid>
      ) : (
        <>
          <Grid item xs={12}>
            <Button onClick={handleSelectAll}>
              {selected.length === data.length ? t('channel_index.unselectAll') : t('channel_index.selectAll')}
            </Button>
          </Grid>
          <Grid item xs={12}>
            {data.map((item) => (
              <FormControlLabel
                key={item.id}
                control={<Checkbox checked={selected.includes(item.id)} onChange={() => handleSelect(item.id)} />}
                label={item.name + '(' + azureBatchAPILabelOther(item.other) + ')'}
              />
            ))}
          </Grid>
          <Grid item xs={12}>
            <TextField
              sx={{ ml: 1, flex: 1 }}
              placeholder={t('channel_index.replaceValue')}
              inputProps={{ 'aria-label': t('channel_index.replaceValue') }}
              value={replaceValue}
              onChange={(e) => {
                setReplaceValue(e.target.value);
              }}
              InputProps={{
                endAdornment: (
                  <InputAdornment position="end">
                    <IconButton aria-label="toggle password visibility" onClick={handleSubmit} edge="end">
                      <IconSend />
                    </IconButton>
                  </InputAdornment>
                )
              }}
            />
          </Grid>
        </>
      )}
    </Grid>
  );
};

export default BatchAzureAPI;
