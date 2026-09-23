'use client';

import { useState } from 'react';
import { useTranslations } from 'next-intl';
import { Globe, Plus } from 'lucide-react';
import { PageWrapper } from '@/components/common/PageWrapper';
import { Button } from '@/components/ui/button';
import { useProxyList, type Proxy } from '@/api/endpoints/proxy';
import { ProxyCard } from './Card';
import { ProxyFormDialog } from './Form';

export function Proxy() {
    const t = useTranslations('proxy');
    const { data: proxies } = useProxyList();
    const [isDialogOpen, setIsDialogOpen] = useState(false);
    const [editingProxy, setEditingProxy] = useState<Proxy | null>(null);

    const openCreate = () => {
        setEditingProxy(null);
        setIsDialogOpen(true);
    };

    const openEdit = (proxy: Proxy) => {
        setEditingProxy(proxy);
        setIsDialogOpen(true);
    };

    return (
        <div className="h-full min-h-0 overflow-y-auto overscroll-contain rounded-t-3xl">
            <PageWrapper className="space-y-4 pb-24 md:pb-4">
                <div key="header" className="flex items-center justify-between gap-4 rounded-3xl border border-border bg-card p-6">
                    <div className="min-w-0">
                        <h2 className="flex items-center gap-2 text-lg font-bold text-card-foreground">
                            <Globe className="size-5" />
                            {t('title')}
                        </h2>
                        <p className="text-sm text-muted-foreground">{t('description')}</p>
                    </div>
                    <Button type="button" className="shrink-0 rounded-2xl" onClick={openCreate}>
                        <Plus className="size-4" />
                        {t('create')}
                    </Button>
                </div>

                {(proxies?.length ?? 0) === 0 ? (
                    <div key="empty" className="rounded-3xl border border-dashed border-border bg-card/50 p-10 text-center text-sm text-muted-foreground">
                        {t('empty')}
                    </div>
                ) : (
                    <div key="list" className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
                        {(proxies ?? []).map((proxy) => (
                            <ProxyCard key={proxy.id} proxy={proxy} onEdit={() => openEdit(proxy)} />
                        ))}
                    </div>
                )}
            </PageWrapper>

            <ProxyFormDialog open={isDialogOpen} proxy={editingProxy} onOpenChange={setIsDialogOpen} />
        </div>
    );
}
