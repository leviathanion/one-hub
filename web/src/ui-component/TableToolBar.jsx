import PropTypes from 'prop-types';
import { Icon } from '@iconify/react';

import Toolbar from '@mui/material/Toolbar';
import OutlinedInput from '@mui/material/OutlinedInput';
import InputAdornment from '@mui/material/InputAdornment';

import { useTheme } from '@mui/material/styles';

// ----------------------------------------------------------------------

export default function TableToolBar({ placeholder }) {
  const theme = useTheme();
  const grey500 = theme.palette.grey[500];

  return (
    <Toolbar
      sx={{
        minHeight: { xs: 64, sm: 80 },
        height: 'auto',
        display: 'flex',
        flexWrap: 'wrap',
        justifyContent: 'space-between',
        py: { xs: 2, sm: 0 },
        pl: { xs: 2, sm: 3 },
        pr: { xs: 2, sm: 1 }
      }}
    >
      <OutlinedInput
        id="keyword"
        name="keyword"
        sx={{
          width: '100%',
          minWidth: '100%',
          '& .MuiInputAdornment-root:hover': {
            '& .search-icon': {
              borderRadius: '50%',
              boxShadow: '0 0 8px rgba(0,0,0,0.1)',
              transition: 'all 0.3s ease'
            }
          }
        }}
        placeholder={placeholder}
        startAdornment={
          <InputAdornment position="start">
            <Icon icon="solar:minimalistic-magnifer-line-duotone" className="search-icon" width="20" height="20" color={grey500} />
          </InputAdornment>
        }
      />
    </Toolbar>
  );
}

TableToolBar.propTypes = {
  filterName: PropTypes.string,
  handleFilterName: PropTypes.func,
  placeholder: PropTypes.string
};
