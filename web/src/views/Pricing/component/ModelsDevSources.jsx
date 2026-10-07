import { useState } from 'react';
import PropTypes from 'prop-types';
import { Box, Stack, TablePagination, Typography } from '@mui/material';
import { useTranslation } from 'react-i18next';

const pageSize = 25;
const reasonKeys = {
  'ambiguous providers': 'sourceAmbiguous',
  'official provider price is invalid': 'sourceInvalidOfficial',
  'another provider selected': 'sourceAlternative'
};

export default function ModelsDevSources({ candidates }) {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState(false);
  const [page, setPage] = useState(0);
  if (!candidates.length) return null;

  return (
    <Box
      component="details"
      onToggle={(event) => setExpanded(event.currentTarget.open)}
      sx={{ border: 1, borderColor: 'divider', borderRadius: 1.5, p: 1.5 }}
    >
      <Box component="summary" sx={{ cursor: 'pointer', typography: 'subtitle2' }}>
        {t('pricingSync.sourceDetails', { count: candidates.length })}
      </Box>
      {expanded && (
        <>
          <Typography variant="body2" color="text.secondary" sx={{ mt: 1 }}>
            {t('pricingSync.sourceDetailsHelp')}
          </Typography>
          <Stack component="ul" spacing={1.5} sx={{ pl: 2.5, my: 2 }}>
            {candidates.slice(page * pageSize, (page + 1) * pageSize).map((candidate) => (
              <Box component="li" key={`${candidate.provider}/${candidate.model}`} sx={{ overflowWrap: 'anywhere' }}>
                <Typography variant="body2" fontWeight={600}>
                  {candidate.model} · {candidate.provider}
                </Typography>
                <Typography variant="body2" color={candidate.selected ? 'success.main' : 'text.secondary'}>
                  {candidate.selected
                    ? t('pricingSync.sourceSelected')
                    : reasonKeys[candidate.reason]
                      ? t(`pricingSync.${reasonKeys[candidate.reason]}`)
                      : t('pricingSync.sourceRejected', { reason: candidate.reason })}
                </Typography>
              </Box>
            ))}
          </Stack>
          <TablePagination
            component="div"
            count={candidates.length}
            rowsPerPage={pageSize}
            rowsPerPageOptions={[]}
            page={page}
            onPageChange={(_, nextPage) => setPage(nextPage)}
            labelDisplayedRows={({ from, to, count }) => t('pricingSync.sourcePage', { from, to, count })}
            getItemAriaLabel={(type) => t(`pricingSync.sourcePage${type === 'previous' ? 'Previous' : 'Next'}`)}
            sx={{ '& .MuiTablePagination-toolbar': { p: 0, flexWrap: 'wrap' } }}
          />
        </>
      )}
    </Box>
  );
}

ModelsDevSources.propTypes = {
  candidates: PropTypes.arrayOf(
    PropTypes.shape({
      provider: PropTypes.string.isRequired,
      model: PropTypes.string.isRequired,
      selected: PropTypes.bool,
      reason: PropTypes.string
    })
  ).isRequired
};
