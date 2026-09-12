import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import './index.css';
import { ApiError, clearStoredToken, getSession, login } from './lib/api';
import { LoginPage } from './pages/LoginPage';
import { UsagePage } from './pages/UsagePage';

type AuthState = 'checking' | 'authenticated' | 'unauthenticated';

function App() {
  const { t } = useTranslation();
  const [authState, setAuthState] = useState<AuthState>('checking');
  const [loginError, setLoginError] = useState('');
  const [submitting, setSubmitting] = useState(false);

  const loadSession = useCallback(async () => {
    try {
      const session = await getSession();
      setAuthState(session.authenticated ? 'authenticated' : 'unauthenticated');
    } catch {
      clearStoredToken();
      setAuthState('unauthenticated');
    }
  }, []);

  useEffect(() => {
    void loadSession();
  }, [loadSession]);

  const handleLogin = useCallback(async (password: string) => {
    setSubmitting(true);
    setLoginError('');
    try {
      await login(password);
      setAuthState('authenticated');
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        setLoginError(t('auth.invalid_password'));
      } else {
        setLoginError(t('auth.login_failed'));
      }
      setAuthState('unauthenticated');
    } finally {
      setSubmitting(false);
    }
  }, [t]);

  if (authState === 'checking') {
    return <div style={{ minHeight: '100vh', background: 'var(--bg-secondary)' }} aria-busy="true" />;
  }

  if (authState === 'unauthenticated') {
    return <LoginPage loading={submitting} error={loginError} onSubmit={handleLogin} />;
  }

  return (
    <UsagePage
      onAuthRequired={() => {
        clearStoredToken();
        setAuthState('unauthenticated');
      }}
    />
  );
}

export default App;
