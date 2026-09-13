import React, { useState } from 'react';
import PropTypes from 'prop-types';

import Dialog from '@mui/material/Dialog';
import DialogTitle from '@mui/material/DialogTitle';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import { Box, List, Button, ListItem, TextField, IconButton, ListItemSecondaryAction } from '@mui/material';
import { useTheme } from '@mui/material/styles';
import Editor from '@monaco-editor/react';

import { Icon } from '@iconify/react';
import { showError } from 'utils/common';
import { useTranslation } from 'react-i18next';

const JSON_EDITOR_OPTIONS = {
  minimap: { enabled: false },
  scrollBeyondLastLine: false,
  automaticLayout: true,
  fontSize: 14,
  lineNumbers: 'on',
  folding: true,
  formatOnPaste: true,
  formatOnType: true
};

const ListInput = ({ listValue, onChange, disabled, error, label }) => {
  const { t } = useTranslation();
  const theme = useTheme();
  // 受控组件：不再把 props 复制进本地 state，编辑时每次按键少一轮 state 同步渲染。
  const items = Array.isArray(listValue) ? listValue : [];

  const [openJsonDialog, setOpenJsonDialog] = useState(false);
  const [jsonInput, setJsonInput] = useState('');

  const handleAdd = () => {
    onChange([...items, '']);
  };

  const handleDelete = (index) => {
    onChange(items.filter((_, idx) => idx !== index));
  };

  const handleChange = (index, newValue) => {
    const newItems = [...items];
    newItems[index] = newValue;
    onChange(newItems);
  };

  const handleAddByJson = () => {
    // 将当前列表转换为JSON字符串
    const currentItemsJson = JSON.stringify(items, null, 2);
    setJsonInput(currentItemsJson);
    setOpenJsonDialog(true);
  };

  const handleCloseJsonDialog = () => {
    setOpenJsonDialog(false);
    setJsonInput('');
  };

  const handleJsonSubmit = () => {
    try {
      const parsedJson = JSON.parse(jsonInput);
      if (!Array.isArray(parsedJson)) {
        throw new Error(t('channel_edit.listJsonError'));
      }

      onChange(parsedJson.map((item) => item.toString()));
      handleCloseJsonDialog();
    } catch (e) {
      showError(t('channel_edit.listJsonError'));
    }
  };

  return (
    <Box>
      <List>
        {items.map((value, index) => (
          <ListItem key={index}>
            <TextField
              label={label?.itemName || '项目'}
              value={value}
              onChange={(e) => handleChange(index, e.target.value)}
              disabled={disabled}
              error={error}
              sx={{ mr: 1, flex: 1 }}
            />
            <ListItemSecondaryAction>
              <IconButton edge="end" aria-label="delete" onClick={() => handleDelete(index)} disabled={disabled}>
                <Icon icon="mdi:delete" />
              </IconButton>
            </ListItemSecondaryAction>
          </ListItem>
        ))}
      </List>
      <Button startIcon={<Icon icon="mdi:plus" />} onClick={handleAdd} disabled={disabled}>
        {t('channel_edit.mapAdd', { name: label?.name || '项目' })}
      </Button>

      <Button startIcon={<Icon icon="mdi:plus" />} onClick={handleAddByJson} disabled={disabled}>
        {t('channel_edit.mapAddByJson', { name: label?.name || '项目' })}
      </Button>

      <Dialog open={openJsonDialog} onClose={handleCloseJsonDialog} fullWidth maxWidth="md">
        <DialogTitle>{t('channel_edit.mapAddByJson', { name: label?.name || '项目' })}</DialogTitle>
        <DialogContent>
          <Box
            sx={{
              border: '1px solid',
              borderColor: 'divider',
              borderRadius: 1,
              overflow: 'hidden',
              marginTop: 1,
              resize: 'vertical',
              height: '400px',
              minHeight: '200px',
              '&:hover': {
                borderColor: 'primary.main'
              },
              '&:focus-within': {
                borderColor: 'primary.main',
                borderWidth: 1
              }
            }}
          >
            <Editor
              height="100%"
              language="json"
              theme={theme.palette.mode === 'dark' ? 'vs-dark' : 'light'}
              value={jsonInput}
              options={JSON_EDITOR_OPTIONS}
              onChange={setJsonInput}
            />
          </Box>
        </DialogContent>
        <DialogActions>
          <Button onClick={handleCloseJsonDialog}>{t('common.cancel')}</Button>
          <Button onClick={handleJsonSubmit}>{t('common.submit')}</Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
};

ListInput.propTypes = {
  listValue: PropTypes.array,
  onChange: PropTypes.func.isRequired,
  disabled: PropTypes.bool,
  error: PropTypes.bool,
  label: PropTypes.shape({
    name: PropTypes.string,
    itemName: PropTypes.string
  }).isRequired
};

export default ListInput;
