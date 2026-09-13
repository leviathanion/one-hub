import { useLayoutEffect, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { useFormikContext } from 'formik';
import * as Yup from 'yup';
import ChannelForm from '../../src/views/Channel/component/ChannelForm';
import ChannelModelsField from '../../src/views/Channel/component/ChannelModelsField';

const params = new URLSearchParams(location.search);
const count = Number(params.get('n') || 3000);
const selected = Number(params.get('selected') || 0);
const options = Array.from({ length: count }, (_, i) => ({
  id: `model-${String(i).padStart(4, '0')}`,
  group: `group-${Math.floor(i / 1000)}`
}));
const initialValues = {
  name: '测试渠道',
  models: (params.has('last') ? options.slice(count - selected) : options.slice(0, selected)).map((model) => ({ ...model }))
};
const schema = Yup.object({ name: Yup.string().required('名称不能为空'), models: Yup.array().min(1, '至少选择一个模型') });

function ObserveForm() {
  const { values } = useFormikContext();
  useLayoutEffect(() => {
    window.formValues = values;
  }, [values]);
  return null;
}

function App() {
  const [disabled, setDisabled] = useState(false);
  return (
    <div style={{ width: 'min(900px, 95vw)', padding: 12 }}>
      <button id="toggle-disabled" onClick={() => setDisabled((value) => !value)}>
        切换禁用
      </button>
      <ChannelForm
        initialValues={initialValues}
        validationSchema={schema}
        onSubmit={(values, helpers) => {
          window.submitted = values;
          helpers.setSubmitting(false);
        }}
      >
        {({ values, handleChange, handleSubmit, getValues }) => {
          window.layoutRenders = (window.layoutRenders || 0) + 1;
          return (
            <form onSubmit={handleSubmit}>
              <label htmlFor="name">名称</label>
              <input id="name" name="name" value={values.name} onChange={handleChange} />
              <ChannelModelsField
                options={options}
                disabled={disabled}
                label="模型"
                helperText="支持搜索和自定义模型"
                customGroupLabel="自定义"
              />
              <button
                id="snapshot"
                type="button"
                onClick={() => {
                  window.snapshot = getValues();
                }}
              >
                读取最新表单
              </button>
              <button id="save" type="submit">
                保存
              </button>
              <ObserveForm />
            </form>
          );
        }}
      </ChannelForm>
    </div>
  );
}

createRoot(document.getElementById('root')).render(<App />);
