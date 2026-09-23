'use client';

import { useState } from 'react';
import { useTranslations } from 'next-intl';
import {
    Dialog,
    DialogContent,
    DialogDescription,
    DialogFooter,
    DialogHeader,
    DialogTitle,
} from '@/components/ui/dialog';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { toast } from '@/components/common/Toast';
import { useCreateProxy, useUpdateProxy, type Proxy } from '@/api/endpoints/proxy';

interface ProxyFormDialogProps {
    open: boolean;
    proxy: Proxy | null;
    onOpenChange: (open: boolean) => void;
}

export function ProxyFormDialog({ open, proxy, onOpenChange }: ProxyFormDialogProps) {
    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <DialogContent className="rounded-3xl sm:max-w-md">
                <ProxyForm proxy={proxy} onDone={() => onOpenChange(false)} />
            </DialogContent>
        </Dialog>
    );
}

// 表单在 Dialog 打开时挂载、关闭时卸载，因此可直接用 proxy 初始化本地状态。
function ProxyForm({ proxy, onDone }: { proxy: Proxy | null; onDone: () => void }) {
    const t = useTranslations('proxy');
    const createProxy = useCreateProxy();
    const updateProxy = useUpdateProxy();
    const [name, setName] = useState(proxy?.name ?? '');
    const [url, setUrl] = useState(proxy?.url ?? '');
    const [remark, setRemark] = useState(proxy?.remark ?? '');

    const isPending = createProxy.isPending || updateProxy.isPending;

    const handleSubmit = (event: React.FormEvent<HTMLFormElement>) => {
        event.preventDefault();
        const payload = { name: name.trim(), url: url.trim(), remark: remark.trim() };
        if (!payload.name || !payload.url) return;

        const onSuccess = () => {
            toast.success(proxy ? t('updated') : t('created'));
            onDone();
        };
        const onError = (error: unknown) => {
            toast.error(t('saveFailed'), {
                description: error instanceof Error ? error.message : String(error),
            });
        };

        if (proxy) {
            updateProxy.mutate({ id: proxy.id, ...payload }, { onSuccess, onError });
        } else {
            createProxy.mutate(payload, { onSuccess, onError });
        }
    };

    return (
        <>
            <DialogHeader>
                <DialogTitle>{proxy ? t('editTitle') : t('createTitle')}</DialogTitle>
                <DialogDescription>{t('formHint')}</DialogDescription>
            </DialogHeader>
            <form onSubmit={handleSubmit} className="space-y-4">
                <div className="space-y-2">
                    <label htmlFor="proxy-name" className="text-sm font-medium text-card-foreground">{t('name')}</label>
                    <Input
                        id="proxy-name"
                        value={name}
                        onChange={(event) => setName(event.target.value)}
                        placeholder={t('namePlaceholder')}
                        className="rounded-xl"
                        required
                    />
                </div>
                <div className="space-y-2">
                    <label htmlFor="proxy-url" className="text-sm font-medium text-card-foreground">{t('url')}</label>
                    <Input
                        id="proxy-url"
                        value={url}
                        onChange={(event) => setUrl(event.target.value)}
                        placeholder={t('urlPlaceholder')}
                        className="rounded-xl"
                        required
                    />
                </div>
                <div className="space-y-2">
                    <label htmlFor="proxy-remark" className="text-sm font-medium text-card-foreground">{t('remark')}</label>
                    <Input
                        id="proxy-remark"
                        value={remark}
                        onChange={(event) => setRemark(event.target.value)}
                        className="rounded-xl"
                    />
                </div>
                <DialogFooter className="gap-2">
                    <Button type="button" variant="secondary" className="rounded-2xl" onClick={onDone}>
                        {t('cancel')}
                    </Button>
                    <Button type="submit" className="rounded-2xl" disabled={isPending}>
                        {isPending ? t('saving') : t('save')}
                    </Button>
                </DialogFooter>
            </form>
        </>
    );
}
