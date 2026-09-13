import { memo } from 'react';
import PropTypes from 'prop-types';
import { Formik, useFormikContext } from 'formik';
import { shallowEqual } from 'react-redux';
import { useEventCallback } from '@mui/material/utils';

const withoutModels = (fields) => {
  const result = { ...fields };
  delete result.models;
  return result;
};

const Layout = memo(
  function Layout({ children, values, errors, touched, controls }) {
    return children({ values, errors, touched, ...controls });
  },
  (previous, next) =>
    previous.children === next.children &&
    shallowEqual(previous.values, next.values) &&
    shallowEqual(previous.errors, next.errors) &&
    shallowEqual(previous.touched, next.touched) &&
    shallowEqual(previous.controls, next.controls)
);

Layout.propTypes = {
  children: PropTypes.func.isRequired,
  values: PropTypes.object.isRequired,
  errors: PropTypes.object.isRequired,
  touched: PropTypes.object.isRequired,
  controls: PropTypes.object.isRequired
};

// models 的值和错误由 ChannelModelsField 消费。需要完整表单的事件通过 getValues 读取最新值，
// 避免把旧 models 捕获在跳过渲染的按钮回调中；Formik 仍是唯一事实来源。
function ChannelFormLayout({ children }) {
  const formik = useFormikContext();
  const getValues = useEventCallback(() => formik.values);
  const { handleBlur, handleChange, handleSubmit, isSubmitting, setFieldValue } = formik;
  return (
    <Layout
      values={withoutModels(formik.values)}
      errors={withoutModels(formik.errors)}
      touched={withoutModels(formik.touched)}
      controls={{ handleBlur, handleChange, handleSubmit, isSubmitting, setFieldValue, getValues }}
    >
      {children}
    </Layout>
  );
}

ChannelFormLayout.propTypes = { children: PropTypes.func.isRequired };

export default function ChannelForm({ children, ...props }) {
  return (
    <Formik {...props}>
      <ChannelFormLayout>{children}</ChannelFormLayout>
    </Formik>
  );
}

ChannelForm.propTypes = { children: PropTypes.func.isRequired };
