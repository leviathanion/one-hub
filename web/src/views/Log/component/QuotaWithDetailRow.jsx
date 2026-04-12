import { Box, Typography, IconButton } from '@mui/material';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import { renderQuota } from 'utils/common';
import PropTypes from 'prop-types';
import { calculateOriginalQuota, getGroupRatio } from './quotaDetail';

// QuotaWithDetailRow is only responsible for the price in the main row and the small triangle
export default function QuotaWithDetailRow({ item, open, setOpen }) {
  const groupRatio = getGroupRatio(item?.metadata);
  const originalQuota = calculateOriginalQuota(item);
  const quota = item.quota || 0;
  const showOriginalQuota = originalQuota > 0 && Math.abs(originalQuota - quota) > 1e-9;
  const originalQuotaDecoration = quota < originalQuota ? 'line-through' : 'none';

  return (
    <Box sx={{ display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
      <Box onClick={() => setOpen((o) => !o)} sx={{ display: 'flex', flexDirection: 'column', mr: 1, cursor: 'pointer' }}>
        {showOriginalQuota ? (
          <>
            <Typography
              variant="caption"
              sx={{
                color: (theme) => theme.palette.text.secondary,
                textDecoration: originalQuotaDecoration,
                fontSize: 12
              }}
            >
              {renderQuota(originalQuota, 6)}
            </Typography>
            <Typography
              sx={{ color: (theme) => theme.palette[quota > originalQuota ? 'warning' : 'success'].main, fontWeight: 500, fontSize: 13 }}
            >
              {renderQuota(quota, 6)}
            </Typography>
          </>
        ) : (
          <Typography sx={{ color: (theme) => theme.palette[groupRatio > 1 ? 'warning' : 'success'].main, fontWeight: 500, fontSize: 13 }}>
            {renderQuota(quota, 6)}
          </Typography>
        )}
      </Box>
      <IconButton
        size="small"
        onClick={() => setOpen((o) => !o)}
        sx={{
          ml: 0.5,
          bgcolor: (theme) => (open ? theme.palette.action.hover : 'transparent'),
          '&:hover': { bgcolor: (theme) => theme.palette.action.hover }
        }}
      >
        <ExpandMoreIcon
          style={{
            transition: '0.2s',
            transform: open ? 'rotate(180deg)' : 'rotate(0deg)'
          }}
          fontSize="small"
        />
      </IconButton>
    </Box>
  );
}

QuotaWithDetailRow.propTypes = {
  item: PropTypes.shape({
    quota: PropTypes.number,
    metadata: PropTypes.shape({
      group_ratio: PropTypes.number,
      original_quota: PropTypes.number,
      origin_quota: PropTypes.number
    })
  }).isRequired,
  open: PropTypes.bool.isRequired,
  setOpen: PropTypes.func.isRequired
};
