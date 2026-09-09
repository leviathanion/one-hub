import PropTypes from 'prop-types';
import * as Yup from 'yup';
import { Formik } from 'formik';
import { useState, useEffect } from 'react';
import { Alert, Dialog, DialogTitle, DialogContent, DialogActions, Button, Stack, TextField, MenuItem, Typography } from '@mui/material';
import { showSuccess, showError } from 'utils/common';
import { API } from 'utils/api';
import { PaymentType, CurrencyType, PaymentConfig, PaymentProducts, defaultConfig } from '../type/Config';

const initialPayment = () => ({
  type: 'epay',
  name: '',
  icon: '',
  notify_domain: '',
  fixed_fee: 0,
  percent_fee: 0,
  currency: 'CNY',
  default_product: Object.keys(PaymentProducts.epay)[0],
  config: defaultConfig('epay'),
  sort: 0,
  enable: true
});
const schema = Yup.object({
  name: Yup.string().required('请输入名称'),
  icon: Yup.string().required('请输入图标地址'),
  fixed_fee: Yup.number().min(0, '固定手续费不能为负'),
  percent_fee: Yup.number().min(0, '手续费率不能为负'),
  currency: Yup.string().required('请选择币种'),
  default_product: Yup.string().required('请选择支付产品')
});

const EditModal = ({ open, paymentId, onCancel, onOk, onCreated, credentialsOnly = false }) => {
  const [inputs, setInputs] = useState(initialPayment);
  const [rotation, setRotation] = useState(false);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState('');
  useEffect(() => {
    if (!open) return;
    setRotation(credentialsOnly);
    setLoadError('');
    if (!paymentId) {
      setLoading(false);
      setInputs(initialPayment());
      return;
    }
    let active = true;
    setLoading(true);
    API.get(`/api/payment/${paymentId}`)
      .then(({ data }) => {
        if (!data.success) throw new Error(data.message || '读取网关失败');
        if (active) {
          setInputs({ ...data.data, config: JSON.parse(data.data.config) });
          setRotation(credentialsOnly || data.data.setup_status === 'failed');
        }
      })
      .catch((err) => {
        if (active) setLoadError(err.message || '读取网关失败');
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [open, paymentId, credentialsOnly]);

  const submit = async (values, { setErrors, setSubmitting }) => {
    try {
      let response;
      if (paymentId && rotation) {
        response = await API.put(`/api/payment/${paymentId}/credentials`, {
          expected_revision: inputs.credential_revision,
          config: JSON.stringify(values.config)
        });
      } else {
        const business = {
          name: values.name.trim(),
          icon: values.icon.trim(),
          notify_domain: values.notify_domain.trim(),
          fixed_fee: Number(values.fixed_fee),
          percent_fee: Number(values.percent_fee),
          currency: values.currency,
          default_product: values.default_product,
          sort: Number(values.sort),
          enable: values.enable
        };
        response = paymentId
          ? await API.put('/api/payment/', { id: paymentId, ...business })
          : await API.post('/api/payment/', { ...business, type: values.type, config: JSON.stringify(values.config) });
      }
      if (!response.data.success) {
        if (!paymentId && response.data.data?.id) onCreated?.(response.data.data.id);
        throw new Error(response.data.message || '保存失败');
      }
      showSuccess(rotation ? '凭证已轮换' : '支付网关已保存');
      onOk(true);
    } catch (err) {
      const message = err.response?.data?.message || err.message || '保存失败';
      showError(message);
      setErrors({ submit: message });
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog open={open} onClose={onCancel} fullWidth maxWidth="md">
      <DialogTitle>{paymentId ? (rotation ? '轮换支付凭证' : '编辑支付网关') : '创建支付网关'}</DialogTitle>
      <DialogContent>
        {loadError && <Alert severity="error">{loadError}</Alert>}
        {loading ? (
          <Typography>正在读取网关…</Typography>
        ) : (
          !loadError && (
            <Formik initialValues={inputs} enableReinitialize validationSchema={schema} onSubmit={submit}>
              {({ values, errors, handleChange, handleSubmit, setFieldValue, isSubmitting, resetForm }) => (
                <form noValidate onSubmit={handleSubmit}>
                  <Stack spacing={2} sx={{ pt: 1 }}>
                    {paymentId && (
                      <Alert severity="info">
                        商户、应用、环境与协议身份已冻结；更换主体请新建网关。凭证版本：{inputs.credential_revision}；配置状态：
                        {inputs.setup_status || '—'}。
                      </Alert>
                    )}
                    {inputs.setup_status === 'failed' && (
                      <Alert severity="warning">
                        网关 #{inputs.id} 已保留，配置尚未完成（{inputs.setup_error || '请核查配置'}）。请在本网关修正凭证并重新配置。
                      </Alert>
                    )}
                    {credentialsOnly && (
                      <Typography variant="body2">维护原网关凭证供历史订单验证使用；不会重新开放该网关的新订单。</Typography>
                    )}
                    <TextField
                      select
                      label="网关类型"
                      name="type"
                      value={values.type}
                      disabled={Boolean(paymentId)}
                      onChange={(event) => {
                        handleChange(event);
                        setFieldValue('config', defaultConfig(event.target.value));
                        setFieldValue('default_product', Object.keys(PaymentProducts[event.target.value])[0]);
                      }}
                    >
                      {Object.entries(PaymentType).map(([key, name]) => (
                        <MenuItem key={key} value={key}>
                          {name}
                        </MenuItem>
                      ))}
                    </TextField>
                    {[
                      ['name', '名称'],
                      ['icon', '图标地址'],
                      ['notify_domain', '通知域名'],
                      ['fixed_fee', '固定手续费'],
                      ['percent_fee', '手续费率']
                    ].map(([key, label]) => (
                      <TextField
                        key={key}
                        name={key}
                        label={label}
                        value={values[key]}
                        disabled={rotation}
                        type={key.endsWith('fee') ? 'number' : 'text'}
                        inputProps={key.endsWith('fee') ? { step: 'any', min: 0 } : undefined}
                        onChange={handleChange}
                        error={Boolean(errors[key])}
                        helperText={errors[key]}
                      />
                    ))}
                    <TextField select label="币种" name="currency" value={values.currency} disabled={rotation} onChange={handleChange}>
                      {Object.entries(CurrencyType).map(([key, name]) => (
                        <MenuItem key={key} value={key}>
                          {name}
                        </MenuItem>
                      ))}
                    </TextField>
                    <TextField
                      select
                      label="默认支付产品"
                      name="default_product"
                      value={values.default_product || ''}
                      disabled={rotation}
                      onChange={handleChange}
                    >
                      {(inputs.products || Object.keys(PaymentProducts[values.type] || {})).map((key) => (
                        <MenuItem key={key} value={key}>
                          {PaymentProducts[values.type]?.[key] || key}
                        </MenuItem>
                      ))}
                    </TextField>
                    {Object.entries(PaymentConfig[values.type] || {}).map(([key, field]) => (
                      <TextField
                        key={key}
                        label={field.name}
                        name={`config.${key}`}
                        value={values.config[key] || ''}
                        select={field.type === 'select'}
                        multiline={field.type !== 'select'}
                        disabled={field.generated || (Boolean(paymentId) && (!rotation || field.identity))}
                        helperText={field.description}
                        onChange={handleChange}
                      >
                        {field.type === 'select' &&
                          field.options.map((option) => (
                            <MenuItem key={option.value} value={option.value}>
                              {option.name}
                            </MenuItem>
                          ))}
                      </TextField>
                    ))}
                    {inputs.capabilities && (
                      <Alert severity="info">
                        支持币种：{inputs.capabilities.currencies?.join('、')}； 查单：
                        {inputs.capabilities.query_by_merchant_ref || inputs.capabilities.query_by_resource_ref ? '支持' : '不支持'}；
                        关单：{inputs.capabilities.can_close ? '支持' : '不支持'}； 动作：{inputs.capabilities.action_kinds?.join('、')}。
                      </Alert>
                    )}
                    {errors.submit && <Alert severity="error">{errors.submit}</Alert>}
                  </Stack>
                  <DialogActions>
                    {paymentId && !credentialsOnly && (
                      <Button
                        disabled={isSubmitting}
                        onClick={() => {
                          resetForm();
                          setRotation(!rotation);
                        }}
                      >
                        {rotation ? '返回业务编辑' : '轮换同主体凭证'}
                      </Button>
                    )}
                    <Button onClick={onCancel}>取消</Button>
                    <Button type="submit" variant="contained" disabled={isSubmitting}>
                      {rotation ? '确认轮换凭证' : '保存'}
                    </Button>
                  </DialogActions>
                </form>
              )}
            </Formik>
          )
        )}
      </DialogContent>
    </Dialog>
  );
};
EditModal.propTypes = {
  open: PropTypes.bool,
  paymentId: PropTypes.number,
  onCancel: PropTypes.func,
  onOk: PropTypes.func,
  onCreated: PropTypes.func,
  credentialsOnly: PropTypes.bool
};
export default EditModal;
