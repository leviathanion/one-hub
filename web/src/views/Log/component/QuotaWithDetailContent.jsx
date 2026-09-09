import { Box, Stack, Table, TableBody, TableCell, TableContainer, TableHead, TableRow, Typography } from '@mui/material';
import { renderQuota } from 'utils/common';
import { calculatePrice, formatBillingNumber, getBillingCostRows, getTokenBillingDetails } from './quotaDetail';
import BillingRuleDetails from './BillingRuleDetails';
import { useTranslation } from 'react-i18next';
import PropTypes from 'prop-types';

export default function QuotaWithDetailContent({ item }) {
  const { t, i18n } = useTranslation();
  const text = (key, values) => t('logPage.quotaDetail.' + key, values);
  const number = (value) => formatBillingNumber(value, i18n.language);
  const details = getTokenBillingDetails(item.metadata);
  const rows = getBillingCostRows(item);
  const unitPrice = (row) => {
    if (row.ratio === null) return text('notRecorded');
    const formatPrice = (ratio) => (row.kind === 'tool' ? number(ratio) : calculatePrice(ratio, 1, row.kind === 'times'));
    const complete =
      row.baseRatio !== null && row.multipliers?.every((value) => typeof value === 'number' && Number.isFinite(value) && value >= 0);
    const adjustedPrice = formatPrice(row.ratio);
    const price =
      complete && row.multipliers.length > 0
        ? `${formatPrice(row.baseRatio)} ${row.multipliers.map((value) => `× ${number(value)}`).join(' ')} = $${adjustedPrice}`
        : adjustedPrice;
    return (
      <>
        {text(row.kind === 'tokens' ? 'perMillionTokens' : 'perCall', { price })}
        {!complete && (
          <Typography variant="caption" display="block" color="text.secondary">
            {text(row.baseRatio === null ? 'basePriceNotRecorded' : 'multipliersNotRecorded')}
          </Typography>
        )}
      </>
    );
  };

  return (
    <Stack spacing={2} sx={{ m: 2, p: 2, borderRadius: 2, bgcolor: 'background.paper' }}>
      <TableContainer>
        <Table size="small" aria-label={text('costBreakdown')}>
          <caption style={{ captionSide: 'top', textAlign: 'left', padding: '0 0 8px', color: 'inherit', fontWeight: 600 }}>
            {text('costBreakdown')}
          </caption>
          <TableHead>
            <TableRow>
              <TableCell>{text('item')}</TableCell>
              <TableCell>{text('usage')}</TableCell>
              <TableCell align="right">{text('unitPriceCalculation')}</TableCell>
              <TableCell align="right">{text('referenceAmount')}</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {rows.map((row) => (
              <TableRow key={row.key}>
                <TableCell component="th" scope="row">
                  {text(row.label, { defaultValue: row.label })}
                  {row.variant && (
                    <Typography variant="caption" display="block" color="text.secondary">
                      {row.variant}
                    </Typography>
                  )}
                </TableCell>
                <TableCell>
                  {row.count === null
                    ? text('notRecorded')
                    : text(row.kind === 'tokens' ? 'tokenCount' : 'callCount', { countText: number(row.count) })}
                  {row.adjustments?.map((adjustment) => (
                    <Typography key={adjustment.key} variant="caption" display="block" color="text.secondary">
                      {text('includedUsage', {
                        name: text('tokenKinds.' + adjustment.key),
                        countText: number(adjustment.value),
                        price:
                          adjustment.ratio === null
                            ? text('notRecorded')
                            : text('perMillionTokens', { price: calculatePrice(adjustment.ratio, 1, false) })
                      })}
                    </Typography>
                  ))}
                </TableCell>
                <TableCell align="right">{row.notCharged ? '—' : unitPrice(row)}</TableCell>
                <TableCell align="right" sx={{ whiteSpace: 'nowrap' }}>
                  {row.amount === null ? text('notRecorded') : renderQuota(row.amount, 6)}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </TableContainer>
      <Typography variant="caption" color="text.secondary">
        {text('referenceNote')}
      </Typography>
      <BillingRuleDetails item={item} />
      <Box sx={{ pt: 1.5, borderTop: '1px solid', borderColor: 'divider' }}>
        {details && details.charge !== item.quota && (
          <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
            {text('tokenCharge')}: {renderQuota(details.charge, 6)}
          </Typography>
        )}
        <Stack direction="row" justifyContent="space-between" alignItems="baseline" spacing={2}>
          <Typography variant="subtitle2">{text('actualCharge')}</Typography>
          <Typography variant="h5">{renderQuota(item.quota ?? 0, 6)}</Typography>
        </Stack>
      </Box>
    </Stack>
  );
}

QuotaWithDetailContent.propTypes = {
  item: PropTypes.shape({ quota: PropTypes.number, metadata: PropTypes.object }).isRequired
};
