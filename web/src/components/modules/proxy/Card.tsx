'use client';

import { useState } from 'react';
import { useTranslations } from 'next-intl';
import { Loader2, Pencil, Trash2, Wifi, WifiOff } from 'lucide-react';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
    AlertDialog,
    AlertDialogAction,
    AlertDialogCancel,
    AlertDialogContent,
    AlertDialogDescription,
    AlertDialogFooter,
    AlertDialogHeader,
    AlertDialogTitle,
    AlertDialogTrigger,
} from '@/components/ui/alert-dialog';
import { toast } from '@/components/common/Toast';
import { useDeleteProxy, useTestProxy, type Proxy, type ProxyTestResult } from '@/api/endpoints/proxy';

interface ProxyCardProps {
    proxy: Proxy;
    onEdit: () => void;
}

export function ProxyCard({ proxy, onEdit }: ProxyCardProps) {
    const t = useTranslations('proxy');
    const deleteProxy = useDeleteProxy();
    const testProxy = useTestProxy();
    const [result, setResult] = useState<ProxyTestResult | null>(null);

    const handleTest = () => {
        setResult(null);
        testProxy.mutate(proxy.id, { onSuccess: (data) => setResult(data) });
    };

    const handleDelete = () => {
        deleteProxy.mutate(proxy.id, {
            onSuccess: () => toast.success(t('deleted')),
            onError: (error) => toast.error(t('deleteFailed'), {
                description: error instanceof Error ? error.message : String(error),
            }),
        });
    };

    return (
        <div className="flex flex-col gap-3 rounded-3xl border border-border bg-card p-5">
            <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                    <h3 className="truncate font-semibold text-card-foreground">{proxy.name}</h3>
                    <p className="break-all text-xs text-muted-foreground">{proxy.url}</p>
                </div>
                <Badge variant="secondary">#{proxy.id}</Badge>
            </div>

            {proxy.remark && <p className="wrap-break-word text-sm text-muted-foreground">{proxy.remark}</p>}

            <div className="flex flex-wrap items-center gap-1">
                <Button
                    type="button"
                    size="sm"
                    variant="secondary"
                    className="rounded-xl"
                    onClick={handleTest}
                    disabled={testProxy.isPending}
                >
                    {testProxy.isPending
                        ? <Loader2 className="size-4 animate-spin" />
                        : <Wifi className="size-4" />}
                    {t('test')}
                </Button>
                <Button type="button" size="sm" variant="ghost" className="rounded-xl" onClick={onEdit}>
                    <Pencil className="size-4" />
                    {t('edit')}
                </Button>
                <AlertDialog>
                    <AlertDialogTrigger asChild>
                        <Button type="button" size="sm" variant="ghost" className="rounded-xl text-destructive hover:text-destructive">
                            <Trash2 className="size-4" />
                            {t('delete')}
                        </Button>
                    </AlertDialogTrigger>
                    <AlertDialogContent className="rounded-3xl">
                        <AlertDialogHeader>
                            <AlertDialogTitle>{t('deleteTitle')}</AlertDialogTitle>
                            <AlertDialogDescription>{t('deleteConfirm', { name: proxy.name })}</AlertDialogDescription>
                        </AlertDialogHeader>
                        <AlertDialogFooter>
                            <AlertDialogCancel className="rounded-2xl">{t('cancel')}</AlertDialogCancel>
                            <AlertDialogAction className="rounded-2xl" onClick={handleDelete}>{t('delete')}</AlertDialogAction>
                        </AlertDialogFooter>
                    </AlertDialogContent>
                </AlertDialog>
            </div>

            {result && (
                result.success ? (
                    <div className="flex items-center gap-2 rounded-xl bg-emerald-500/10 px-3 py-2 text-xs text-emerald-600 dark:text-emerald-400">
                        <Wifi className="size-4 shrink-0" />
                        <span className="break-all">
                            {t('exitIp')}: <span className="font-medium">{result.ip}</span>
                            {result.country && <><span> · {t('country')}: </span><span className="font-medium">{result.country}</span></>}
                            <span> · {result.latency_ms}ms</span>
                        </span>
                    </div>
                ) : (
                    <div className="flex items-start gap-2 rounded-xl bg-destructive/10 px-3 py-2 text-xs text-destructive">
                        <WifiOff className="mt-0.5 size-4 shrink-0" />
                        <span className="break-all">{result.error || t('testFailed')}</span>
                    </div>
                )
            )}
        </div>
    );
}
